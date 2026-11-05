//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package thread_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/thread"
	"seyal/internal/workitem"
)

// TestThreadTellsTheStory runs a job from start to approval, with messages in between, and checks
// that its thread reads like the story of what happened.
func TestThreadTellsTheStory(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, sales := testdb.NewTenant(t, owner, "Thread Test")
	var ops, maker string
	err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
		for _, stmt := range []string{
			"UPDATE persons SET display_name = 'Sam Sales' WHERE id::text = $2 AND tenant_id = $1",
			"INSERT INTO roles (tenant_id, key, name) VALUES ($1, 'sales', 'Sales'), ($1, 'ops', 'Ops')",
			"INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, 'sales')",
		} {
			args := []any{tenant}
			if strings.Contains(stmt, "$2") {
				args = append(args, sales)
			}
			if _, err := tx.Exec(ctx, stmt, args...); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, "INSERT INTO persons (tenant_id, display_name) VALUES ($1, 'Olga Ops') RETURNING id::text",
			tenant).Scan(&ops); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, 'ops')", tenant, ops); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "INSERT INTO persons (tenant_id, display_name) VALUES ($1, 'Mia Maker') RETURNING id::text",
			tenant).Scan(&maker)
	})
	if err != nil {
		t.Fatal(err)
	}

	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	store := media.Disk{Dir: t.TempDir()}
	by := ledger.PersonActor(sales)
	step := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, app, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}

	var pv string
	step(func(tx pgx.Tx) error {
		var err error
		if pv, _, err = playbook.Register(ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		if _, _, err = master.Save(ctx, tx, lib.Masters["client"], "acme", "Acme Traders", nil, by); err != nil {
			return err
		}
		_, _, err = master.Save(ctx, tx, lib.Masters["work_type"], "monthly-books", "Monthly bookkeeping",
			engine.Fields{"standard_fee": 1200000.0, "maker": maker, "is_new": false}, by)
		return err
	})

	var job workitem.Item
	step(func(tx pgx.Tx) error {
		var err error
		job, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by,
			Fields: engine.Fields{"client": "acme", "nature_of_work": "monthly-books", "reason": "Monthly books",
				"fee_agreed": 1200000.0, "integrity_check": "pass"}})
		return err
	})
	step(func(tx pgx.Tx) error {
		_, _, err := channel.Receive(ctx, tx, store, channel.Incoming{Channel: "web", PersonID: sales, ThreadID: job.ID,
			Text:  "Client will send bank statements on Friday",
			Files: []channel.File{{ContentType: "audio/webm", Data: []byte("pretend voice note")}}})
		return err
	})
	step(func(tx pgx.Tx) error {
		_, err := workitem.SetFields(ctx, tx, job.ID, engine.Fields{"advance": 300000.0}, by)
		return err
	})
	step(func(tx pgx.Tx) error { _, err := workitem.Act(ctx, tx, job.ID, "submit", "", by); return err })
	step(func(tx pgx.Tx) error {
		_, err := workitem.Decide(ctx, tx, job.ID, sales, "sales", engine.Approve, "")
		return err
	})
	step(func(tx pgx.Tx) error {
		_, err := channel.Post(ctx, tx, job.ID, "Waiting for Ops to approve.")
		return err
	})
	step(func(tx pgx.Tx) error {
		_, err := workitem.Decide(ctx, tx, job.ID, ops, "ops", engine.Approve, "")
		return err
	})

	var entries []thread.Entry
	step(func(tx pgx.Tx) error {
		var err error
		entries, err = thread.For(ctx, tx, job.ID)
		return err
	})

	want := []struct{ who, text string }{
		{"Sam Sales", "started J-"},
		{"Sam Sales", "Client will send bank statements on Friday"},
		{"Sam Sales", "changed Advance"},
		{"Sam Sales", "submit: draft → pending approval"},
		{"Sam Sales", "approved as sales"},
		{"Seyal", "Waiting for Ops to approve."},
		{"Olga Ops", "approved as ops"},
		{"Seyal", "all approvals are in: pending approval → approved"},
		{"Seyal", "Approved. Over to Mia Maker."},
		{"Seyal", "assigned it to Mia Maker"},
	}
	if len(entries) != len(want) {
		for _, e := range entries {
			t.Logf("%s: %s", e.Who, e.Text)
		}
		t.Fatalf("thread has %d entries, want %d", len(entries), len(want))
	}
	for i, w := range want {
		if e := entries[i]; e.Who != w.who || !strings.Contains(e.Text, w.text) {
			t.Errorf("entry %d = %s: %q, want %s: %q", i+1, e.Who, e.Text, w.who, w.text)
		}
	}
	if voice := entries[1].Message; voice == nil || len(voice.Media) != 1 || voice.Media[0].ContentType != "audio/webm" {
		t.Errorf("the message with the voice note = %+v, want its file attached", voice)
	}
}

// TestThreadUsesTheSubjectIndex checks that the thread's ledger query finds one item's events
// through the ledger_subject index (D70), instead of reading every event of the business. A query
// with an OR whose other branch has no index of its own can't use it.
func TestThreadUsesTheSubjectIndex(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, _ := testdb.NewTenant(t, owner, "Thread Index Test")
	var plan []string
	err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		// Tables in the test database are small, so Postgres would often read them whole anyway.
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+thread.EventsQuery, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			return err
		}
		plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The old query (with the OR) also named ledger_subject, but looked up only its tenant_id part,
	// so it still read every event of the business. The lookup must include subject_id.
	text := strings.Join(plan, "\n")
	looksUpItem := false
	for _, line := range plan {
		if strings.Contains(line, "Index Cond") && strings.Contains(line, "subject_id =") {
			looksUpItem = true
		}
	}
	if !strings.Contains(text, "ledger_subject") || !looksUpItem {
		t.Errorf("the thread query doesn't look the item up in the ledger_subject index; plan:\n%s", text)
	}
}
