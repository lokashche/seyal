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
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
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

	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	by := ledger.PersonActor(sales)
	err = db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
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
		{"link removed", "", "DELETE FROM work_item_links WHERE from_item_id = $1", "work_item_links", "missing"},
		{"standard fee changed in master data", "", `UPDATE master_items SET fields = '{"standard_fee": 1}' WHERE key = 'monthly-books' AND $1::text <> ''`,
			"master_items", "differs from the ledger"},
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
