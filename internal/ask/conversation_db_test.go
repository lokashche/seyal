//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/audit"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
)

type office struct {
	t              *testing.T
	ctx            context.Context
	app            *pgxpool.Pool
	tenant         string
	pv             string
	store          media.Disk
	ai             *ai.Setup // AI is off unless a test sets a (fake) model
	sam, olga, mia string    // Sam (sales), Olga (ops), Mia (maker, no approval role)
}

func (o *office) env() ask.Env { return ask.Env{Store: o.store, AI: o.ai} }

func newOffice(t *testing.T) *office {
	t.Helper()
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, sam := testdb.NewTenant(t, owner, "Chat Test")
	o := &office{t: t, ctx: ctx, app: app, tenant: tenant, sam: sam, store: media.Disk{Dir: t.TempDir()}, ai: &ai.Setup{}}

	err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO roles (tenant_id, key, name) VALUES ($1, 'sales', 'Sales'), ($1, 'ops', 'Ops')", tenant); err != nil {
			return err
		}
		add := func(name, role string) (id string, err error) {
			if err = tx.QueryRow(ctx, "INSERT INTO persons (tenant_id, display_name) VALUES ($1, $2) RETURNING id::text",
				tenant, name).Scan(&id); err != nil || role == "" {
				return id, err
			}
			_, err = tx.Exec(ctx, "INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, $3)", tenant, id, role)
			return id, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, 'sales')", tenant, sam); err != nil {
			return err
		}
		var err error
		if o.olga, err = add("Olga Ops", "ops"); err != nil {
			return err
		}
		o.mia, err = add("Mia Maker", "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}

	by := ledger.PersonActor(sam)
	o.in(func(tx pgx.Tx) error {
		if err := playbook.EnsureKinds(ctx, tx, lib, lib.Playbooks["job-evaluation"], by); err != nil {
			return err
		}
		var err error
		if o.pv, _, err = playbook.Register(ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		if _, _, err = master.Save(ctx, tx, lib.Masters["client"], "acme", "Acme Traders", nil, by); err != nil {
			return err
		}
		_, _, err = master.Save(ctx, tx, lib.Masters["work_type"], "monthly-books", "Monthly bookkeeping",
			engine.Fields{"standard_fee": 1200000.0, "maker": o.mia, "is_new": false}, by)
		return err
	})
	return o
}

func (o *office) in(fn func(tx pgx.Tx) error) {
	o.t.Helper()
	if err := db.WithTenant(o.ctx, o.app, o.tenant, fn); err != nil {
		o.t.Fatal(err)
	}
}

// says checks that Seyal's replies contain each wanted text, in order.
func (o *office) says(r ask.Reply, want ...string) {
	o.t.Helper()
	var got []string
	for _, m := range r.Seyal {
		got = append(got, m.Text)
	}
	if len(got) != len(want) {
		o.t.Fatalf("Seyal said %q, want %d replies containing %q", got, len(want), want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			o.t.Fatalf("Seyal said %q, want it to contain %q", got[i], want[i])
		}
	}
}

func (o *office) write(item, person, text string) ask.Reply {
	o.t.Helper()
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Converse(o.ctx, tx, o.env(), item, channel.Incoming{Channel: "web", PersonID: person, Text: text})
		return err
	})
	return r
}

// TestFillAJobByChat runs the whole conversation: Seyal asks, people answer in their own words,
// mistakes get a second chance, and short commands submit and approve the job.
func TestFillAJobByChat(t *testing.T) {
	o := newOffice(t)

	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(o.ctx, tx, o.env(), o.pv, channel.Incoming{Channel: "web", PersonID: o.sam,
			Text: "New monthly bookkeeping job for Acme"})
		return err
	})
	job := r.Item.ID
	o.says(r, "Which client is this for?")

	o.says(o.write(job, o.sam, "acme"), "What kind of work is it?")
	o.says(o.write(job, o.sam, "monthly"), "Why does the client need this?")
	o.says(o.write(job, o.sam, "Monthly books for the year"), "What fee did we agree?")
	o.says(o.write(job, o.sam, "lots"), "Sorry, I didn't get that: I need a number", "What fee did we agree?")
	o.says(o.write(job, o.sam, "12k"), "Did the integrity check pass? (pass / fail)")
	r = o.write(job, o.sam, "passed")
	o.says(r, `Everything needed is filled in. Say "submit"`)

	f := r.Item.Fields
	if f["client"] != "acme" || f["nature_of_work"] != "monthly-books" || f["fee_agreed"] != 1200000.0 ||
		f["integrity_check"] != "pass" || f["standard_fee"] != 1200000.0 {
		t.Fatalf("fields after the chat = %v", f)
	}

	o.says(o.write(job, o.sam, "Called them about the bank statements")) // a note: no reply
	o.says(o.write(job, o.sam, "submit"), "Noted. Waiting for: sales, ops")
	o.says(o.write(job, o.mia, "approve"), "isn't waiting for a decision from you")
	o.says(o.write(job, o.olga, "reject"), "Please add a reason")
	o.says(o.write(job, o.olga, "approve"), "Noted. Waiting for: sales")
	r = o.write(job, o.sam, "approve")
	if r.Item.State != "approved" || r.Item.AssigneeID != o.mia {
		t.Errorf("after both approvals: state %s, assignee %s; want approved and assigned to Mia", r.Item.State, r.Item.AssigneeID)
	}

	o.in(func(tx pgx.Tx) error {
		rep, err := ledger.Verify(o.ctx, tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(o.ctx, tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}

func TestRepeatedMessageGetsNoSecondReply(t *testing.T) {
	o := newOffice(t)
	var job string
	o.in(func(tx pgx.Tx) error {
		r, err := ask.Begin(o.ctx, tx, o.env(), o.pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: "New job"})
		job = r.Item.ID
		return err
	})
	send := func() ask.Reply {
		var r ask.Reply
		o.in(func(tx pgx.Tx) error {
			var err error
			r, err = ask.Converse(o.ctx, tx, o.env(), job, channel.Incoming{Channel: "web", ExternalID: "m-1",
				PersonID: o.sam, Text: "acme"})
			return err
		})
		return r
	}
	o.says(send(), "What kind of work is it?")
	o.says(send()) // same message delivered again: no new answer, no new question
}
