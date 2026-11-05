//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package setup_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/setup"
	"seyal/internal/testdb"
)

func TestImportDemoBusiness(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := setup.Load("../../examples/seed/demo.yaml", lib)
	if err != nil {
		t.Fatal(err)
	}
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10]) // a fresh business for each run
	// A login email is unique across the server, so each run's people get their own emails.
	email := func(key string) string { return key + "@" + f.Business.Slug + ".kaveri-demo.example" }
	for i, p := range f.People {
		f.People[i].Email = email(p.Key)
	}

	first, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Changes == 0 {
		t.Fatalf("first import = %+v, want a new business with changes", first)
	}
	in := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, app, first.TenantID, fn); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("settings, people and master data are in", func(t *testing.T) {
		in(func(tx pgx.Tx) error {
			var currency, zone string
			var month, people, roles int
			if err := tx.QueryRow(ctx, "SELECT currency, timezone, fiscal_year_start_month FROM tenants WHERE id = current_tenant()").
				Scan(&currency, &zone, &month); err != nil {
				return err
			}
			if currency != "INR" || zone != "Asia/Kolkata" || month != 4 {
				t.Errorf("settings = %s %s %d", currency, zone, month)
			}
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&people); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM person_roles").Scan(&roles); err != nil {
				return err
			}
			if people != 10 || roles != 10 {
				t.Errorf("people = %d, role grants = %d; want 10 and 10", people, roles)
			}
			books, err := master.Get(ctx, tx, "work_type", "monthly-books")
			if err != nil {
				return err
			}
			if books.Fields["standard_fee"] != 1200000.0 || books.Fields["maker"] != first.People["karthik"] {
				t.Errorf("monthly bookkeeping = %v; want fee 12,000.00 (1200000) and Karthik as maker", books.Fields)
			}
			return nil
		})
	})

	t.Run("it has the processes and lists it chose", func(t *testing.T) {
		in(func(tx pgx.Tx) error {
			inUse, err := playbook.ListInUse(ctx, tx)
			if err != nil {
				return err
			}
			kinds, err := playbook.Kinds(ctx, tx)
			if len(inUse) != 1 || inUse[0].PlaybookID != "job-evaluation" || len(kinds) != 2 ||
				kinds["client"] == nil || kinds["work_type"] == nil {
				t.Errorf("processes %v, lists %v; want job-evaluation with client and work_type", inUse, kinds)
			}
			return err
		})
	})

	t.Run("importing again changes nothing", func(t *testing.T) {
		var before, after int64
		in(func(tx pgx.Tx) error { return tx.QueryRow(ctx, "SELECT max(seq) FROM ledger").Scan(&before) })
		again, err := setup.Import(ctx, owner, app, f)
		if err != nil {
			t.Fatal(err)
		}
		in(func(tx pgx.Tx) error { return tx.QueryRow(ctx, "SELECT max(seq) FROM ledger").Scan(&after) })
		if again.Created || again.Changes != 0 || after != before {
			t.Errorf("second import = %+v, ledger %d → %d; want no changes", again, before, after)
		}
	})

	t.Run("a changed fee is updated", func(t *testing.T) {
		f.Master["work_type"][0].Fields["standard_fee"] = 15000
		again, err := setup.Import(ctx, owner, app, f)
		if err != nil || again.Changes != 1 {
			t.Errorf("import after a fee change = %+v, %v; want exactly 1 change", again, err)
		}
	})

	t.Run("demo people can log in with a new password", func(t *testing.T) {
		password, err := setup.NewPassword(ctx, app, email("priya"), first.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		var token string
		if err := db.WithServer(ctx, app, func(tx pgx.Tx) error {
			var err error
			token, _, err = auth.LogIn(ctx, tx, email("priya"), password, "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		in(func(tx pgx.Tx) error {
			s, err := auth.Check(ctx, tx, token, "test")
			if err == nil && (s.Name != "Priya Raman" || len(s.Roles) != 1 || s.Roles[0] != "ops") {
				t.Errorf("logged in as %+v, want Priya Raman with the ops role", s)
			}
			return err
		})
		again, err := setup.NewPassword(ctx, app, email("priya"), first.TenantID)
		if err != nil || again == password {
			t.Errorf("a second new password = %v (same as the first: %v)", err, again == password)
		}
		in(func(tx pgx.Tx) error { // each new password is recorded in the business, by Seyal
			var n int
			err := tx.QueryRow(ctx, `
				SELECT count(*) FROM ledger l JOIN persons p ON p.tenant_id = l.tenant_id AND p.id = l.subject_id
				WHERE l.kind = 'person.login_set' AND l.actor_kind = 'system' AND p.login_email = $1`, email("priya")).Scan(&n)
			if err == nil && n != 2 {
				t.Errorf("person.login_set events for her = %d, want 2", n)
			}
			return err
		})
	})

	t.Run("a new password for someone who isn't in the business changes nothing", func(t *testing.T) {
		_, err := setup.NewPassword(ctx, app, email("nobody"), first.TenantID)
		if err == nil {
			t.Fatal("a new password for an email nobody in the business has was made")
		}
		var n int
		if err := db.WithServer(ctx, app, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE email = $1", email("nobody")).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("logins for the email = %d, %v; want none", n, err)
		}
	})

	t.Run("ledger and tables agree", func(t *testing.T) {
		in(func(tx pgx.Tx) error {
			rep, err := ledger.Verify(ctx, tx, ledger.Checkpoint{})
			if err != nil {
				return err
			}
			if !rep.OK() {
				t.Errorf("ledger problems: %v", rep.Problems)
			}
			problems, err := audit.CheckTables(ctx, tx)
			if len(problems) > 0 {
				t.Errorf("table problems: %v", problems)
			}
			return err
		})
	})
}
