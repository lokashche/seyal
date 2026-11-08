//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package audit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/audit"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

// business is a fictional tenant with some history: two linked work items, one approval, master
// data and a registered playbook.
type business struct {
	owner, app *pgxpool.Pool
	tenant     string
	item       string // id of an item waiting for approvals, approved by sales
}

func newBusiness(t *testing.T) business {
	t.Helper()
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, sales := testdb.NewTenant(t, owner, "Audit Test")
	b := business{owner: owner, app: app, tenant: tenant}

	err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO roles (tenant_id, key, name) VALUES ($1, 'sales', 'Sales')", tenant); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, 'sales')",
			tenant, sales)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	by := ledger.PersonActor(sales)
	err = db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		if err := playbook.EnsureKinds(ctx, tx, lib, lib.Playbooks["job-evaluation"], by); err != nil {
			return err
		}
		pv, _, err := playbook.Register(ctx, tx, lib.Playbooks["job-evaluation"])
		if err != nil {
			return err
		}
		if _, _, err := master.Save(ctx, tx, lib.Masters["client"], "acme", "Acme Traders", nil, by); err != nil {
			return err
		}
		if _, _, err := master.Save(ctx, tx, lib.Masters["work_type"], "monthly-books", "Monthly bookkeeping",
			engine.Fields{"standard_fee": 1200000.0, "is_new": false}, by); err != nil {
			return err
		}
		it, err := workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by,
			Fields: engine.Fields{"client": "acme", "nature_of_work": "monthly-books", "reason": "Books",
				"fee_agreed": 1200000.0, "integrity_check": "pass"}})
		if err != nil {
			return err
		}
		if it, err = workitem.Act(ctx, tx, it.ID, "submit", "", by); err != nil {
			return err
		}
		if _, err = workitem.Decide(ctx, tx, it.ID, sales, "sales", engine.Approve, ""); err != nil {
			return err
		}
		b.item = it.ID
		_, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "event", Parent: it.ID,
			By: ledger.SystemActor})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (b business) check(t *testing.T) []audit.Problem {
	t.Helper()
	var problems []audit.Problem
	err := db.WithTenant(context.Background(), b.app, b.tenant, func(tx pgx.Tx) error {
		var err error
		problems, err = audit.CheckTables(context.Background(), tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

func TestTablesMatchLedger(t *testing.T) {
	b := newBusiness(t)
	if problems := b.check(t); len(problems) != 0 {
		t.Errorf("a business used only through Seyal has problems: %v", problems)
	}
}

func TestTamperingWithTablesIsFound(t *testing.T) {
	tests := []struct {
		name   string
		pre    string // optional statement run first; $1 is the item id
		tamper string // run as the owner role, bypassing Seyal; $1 is the item id
		table  string
		want   string
	}{
		{"approved without approvals", "", "UPDATE work_items SET state = 'approved' WHERE id = $1", "work_items", "state is approved"},
		{"fee changed quietly", "", `UPDATE work_items SET fields = fields || '{"fee_agreed": 100}' WHERE id = $1`, "work_items", "fields differ"},
		// The database refuses a second decision by the same person, so the attacker adds a fake person.
		{"approval added outside Seyal",
			"INSERT INTO persons (tenant_id, display_name) SELECT tenant_id, 'Fake Approver' FROM work_items WHERE id = $1",
			`INSERT INTO approvals (tenant_id, work_item_id, item_version, fields_sha256, slot, person_id, decision)
			SELECT a.tenant_id, a.work_item_id, a.item_version, a.fields_sha256, 'ops', p.id, 'approve'
			FROM approvals a, persons p WHERE a.work_item_id = $1 AND a.slot = 'sales' AND p.display_name = 'Fake Approver'`,
			"approvals", "not in the ledger"},
		// The three hand-over columns (D70) are checked like the others.
		{"hand-over note added", "", "UPDATE work_items SET handoff_note = 'quietly added' WHERE id = $1", "work_items", "hand-over note is quietly added"},
		{"held since changed", "", "UPDATE work_items SET held_since = now() WHERE id = $1", "work_items", "held since is 20"},
		{"handed over by changed", "", "UPDATE work_items SET handoff_from = created_by WHERE id = $1", "work_items", "handed over by is"},
		// So are the four step-stay columns (D76): a reminder or an escalation added or wiped outside Seyal.
		{"step stay moved", "", "UPDATE work_items SET step_since = step_since - interval '3 days' WHERE id = $1", "work_items", "in this step since is 20"},
		{"reminder added", "", "UPDATE work_items SET reminded_at = now() WHERE id = $1", "work_items", "reminded at is 20"},
		{"reminder count changed", "", "UPDATE work_items SET reminders = 2 WHERE id = $1", "work_items", "reminders is 2"},
		{"escalation added", "", "UPDATE work_items SET escalated_at = now() WHERE id = $1", "work_items", "escalated at is 20"},
		{"link removed", "", "DELETE FROM work_item_links WHERE from_item_id = $1", "work_item_links", "missing"},
		{"standard fee changed in master data", "", `UPDATE master_items SET fields = '{"standard_fee": 1}' WHERE key = 'monthly-books' AND $1::text <> ''`,
			"master_items", "differs from the ledger"},
		{"list definition edited", "", `UPDATE master_kinds SET source = source || '  - { key: secret, label: Secret, type: text }' || chr(10) WHERE kind = 'client' AND $1::text <> ''`,
			"master_kinds", "changed outside Seyal"},
		{"playbook rules edited", "", `UPDATE playbook_versions SET source = replace(source, 'role: ops', 'role: sales') WHERE $1::text <> ''`,
			"playbook_versions", "text was changed"},
		// The child item is the only other item; its link must go first, as an attacker would do.
		{"item deleted", "DELETE FROM work_item_links WHERE from_item_id = $1", "DELETE FROM work_items WHERE id <> $1",
			"work_items", "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBusiness(t)
			err := db.WithTenant(context.Background(), b.owner, b.tenant, func(tx pgx.Tx) error {
				if tt.pre != "" {
					if _, err := tx.Exec(context.Background(), tt.pre, b.item); err != nil {
						return err
					}
				}
				tag, err := tx.Exec(context.Background(), tt.tamper, b.item)
				if err == nil && tag.RowsAffected() == 0 {
					t.Fatal("test setup: the tampering changed nothing")
				}
				return err
			})
			if err != nil {
				t.Fatalf("tamper: %v", err)
			}
			for _, p := range b.check(t) {
				if p.Table == tt.table && strings.Contains(p.Reason, tt.want) {
					return
				}
			}
			t.Errorf("problems = %v, want one in %s mentioning %q", b.check(t), tt.table, tt.want)
		})
	}
}

// TestSharedMarkIsChecked: `seyal verify` compares how a message came in (messages.via, D93) with its
// message.received event: the "shared from another app" mark can't be taken off, or put on, quietly.
func TestSharedMarkIsChecked(t *testing.T) {
	for _, tt := range []struct {
		name   string
		via    string
		tamper string
		want   string
	}{
		{"mark taken off", channel.ViaShare, "UPDATE messages SET via = NULL WHERE id = $1", `how it came in is "", but the ledger says "share"`},
		{"mark put on", "", "UPDATE messages SET via = 'share' WHERE id = $1", `how it came in is "share", but the ledger says ""`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := newBusiness(t)
			ctx := context.Background()
			var msg string
			err := db.WithTenant(ctx, b.app, b.tenant, func(tx pgx.Tx) error {
				var sam string
				if err := tx.QueryRow(ctx, "SELECT id::text FROM persons LIMIT 1").Scan(&sam); err != nil {
					return err
				}
				m, _, err := channel.Receive(ctx, tx, media.Disk{Dir: t.TempDir()}, channel.Incoming{Channel: "web", PersonID: sam,
					ThreadID: b.item, Text: "We will send the statements tomorrow", Via: tt.via})
				msg = m.ID
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if problems := b.check(t); len(problems) != 0 {
				t.Fatalf("before tampering: %v", problems)
			}
			if err := db.WithTenant(ctx, b.owner, b.tenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tt.tamper, msg)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, p := range b.check(t) {
				if p.Table == "messages" && strings.Contains(p.Reason, tt.want) {
					return
				}
			}
			t.Errorf("problems = %v, want one in messages mentioning %q", b.check(t), tt.want)
		})
	}
}
