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
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := setup.Load("../../examples/seed/demo.yaml", lib)
	if err != nil {
		t.Fatal(err)
	}
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10]) // a fresh business for each run

	first, err := setup.Import(ctx, owner, app, lib, f)
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
			if people != 6 || roles != 5 {
				t.Errorf("people = %d, role grants = %d; want 6 and 5", people, roles)
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

	t.Run("importing again changes nothing", func(t *testing.T) {
		var before, after int64
		in(func(tx pgx.Tx) error { return tx.QueryRow(ctx, "SELECT max(seq) FROM ledger").Scan(&before) })
		again, err := setup.Import(ctx, owner, app, lib, f)
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
		again, err := setup.Import(ctx, owner, app, lib, f)
		if err != nil || again.Changes != 1 {
			t.Errorf("import after a fee change = %+v, %v; want exactly 1 change", again, err)
		}
	})

	t.Run("demo people can log in with a new password", func(t *testing.T) {
		password, err := setup.NewPassword(ctx, app, first.TenantID, "priya@kaveri-demo.example")
		if err != nil {
			t.Fatal(err)
		}
		in(func(tx pgx.Tx) error {
			_, s, err := auth.Login(ctx, tx, "priya@kaveri-demo.example", password, "test")
			if err == nil && (s.Name != "Priya Raman" || len(s.Roles) != 1 || s.Roles[0] != "ops") {
				t.Errorf("logged in as %+v, want Priya Raman with the ops role", s)
			}
			return err
		})
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
