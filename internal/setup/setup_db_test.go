//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package setup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
			var daysOff []int16
			var paused bool
			if err := tx.QueryRow(ctx, `SELECT currency, timezone, fiscal_year_start_month, days_off, chase_paused
				FROM tenants WHERE id = current_tenant()`).Scan(&currency, &zone, &month, &daysOff, &paused); err != nil {
				return err
			}
			if currency != "INR" || zone != "Asia/Kolkata" || month != 4 || len(daysOff) != 1 || daysOff[0] != 0 || paused {
				t.Errorf("settings = %s %s %d, days off %v, paused %v; want Sunday off and chasing on", currency, zone, month, daysOff, paused)
			}
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&people); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM person_roles").Scan(&roles); err != nil {
				return err
			}
			if people != 11 || roles != 11 {
				t.Errorf("people = %d, role grants = %d; want 11 and 11", people, roles)
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
			var ids []string
			for _, p := range inUse {
				ids = append(ids, p.PlaybookID)
			}
			slices.Sort(ids)
			// Other work, the catch-all, is in the demo for the "move someone's work" story (D75, M3), and
			// the recurring Monthly return for recurring work (D75, R).
			if !slices.Equal(ids, []string{"job-evaluation", "monthly-return", "other-work"}) || len(kinds) != 2 ||
				kinds["client"] == nil || kinds["work_type"] == nil {
				t.Errorf("processes %v, lists %v; want job-evaluation, monthly-return and other-work, with client and work_type", ids, kinds)
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

	t.Run("changed working days and the pause are recorded", func(t *testing.T) {
		f.Business.DaysOff = []string{"saturday", "Sunday"}
		f.Business.Holidays = []string{"2026-11-09"}
		f.Business.Chase = "paused"
		again, err := setup.Import(ctx, owner, app, f)
		if err != nil || again.Changes != 1 {
			t.Fatalf("import after new working days = %+v, %v; want exactly 1 change", again, err)
		}
		in(func(tx pgx.Tx) error {
			var daysOff []int16
			var holidays []string
			var paused bool
			if err := tx.QueryRow(ctx, `SELECT days_off, holidays::text[], chase_paused FROM tenants WHERE id = current_tenant()`).
				Scan(&daysOff, &holidays, &paused); err != nil {
				return err
			}
			if len(daysOff) != 2 || daysOff[0] != 0 || daysOff[1] != 6 || len(holidays) != 1 || holidays[0] != "2026-11-09" || !paused {
				t.Errorf("stored days off %v, holidays %v, paused %v", daysOff, holidays, paused)
			}
			var n int
			var change string
			if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(body::jsonb #>> '{payload,changes}'), '') FROM ledger
				WHERE kind = 'business.updated' AND actor_kind = 'system' AND subject_id::text = current_tenant()::text`).Scan(&n, &change); err != nil {
				return err
			}
			for _, want := range []string{`"days_off": {"new": ["sunday", "saturday"], "old": ["sunday"]}`,
				`"holidays": {"new": ["2026-11-09"], "old": []}`, `"chase_paused": {"new": true, "old": false}`} {
				if !strings.Contains(change, want) {
					t.Errorf("business.updated changes = %s; want %s", change, want)
				}
			}
			if n != 1 || strings.Contains(change, "timezone") {
				t.Errorf("%d business.updated events, changes %s; want one, with only what changed", n, change)
			}
			return nil
		})
		if again, err := setup.Import(ctx, owner, app, f); err != nil || again.Changes != 0 {
			t.Errorf("the same file again = %+v, %v; want no changes", again, err)
		}
	})

	t.Run("the phone-note switch and hours are recorded", func(t *testing.T) {
		in(func(tx pgx.Tx) error {
			var on bool
			var from, until string
			if err := tx.QueryRow(ctx, `SELECT notifications, notify_from, notify_until FROM tenants WHERE id = current_tenant()`).
				Scan(&on, &from, &until); err != nil {
				return err
			}
			if !on || from != "09:00" || until != "19:00" {
				t.Errorf("the demo file stored notifications %v, hours %s-%s; want on, 09:00-19:00", on, from, until)
			}
			return nil
		})
		f.Business.Notifications, f.Business.NotifyHours = "", "08:30-24:00"
		again, err := setup.Import(ctx, owner, app, f)
		if err != nil || again.Changes != 1 {
			t.Fatalf("import with phone notes left out = %+v, %v; want exactly 1 change", again, err)
		}
		in(func(tx pgx.Tx) error {
			var on bool
			var from, until, change string
			if err := tx.QueryRow(ctx, `SELECT notifications, notify_from, notify_until FROM tenants WHERE id = current_tenant()`).
				Scan(&on, &from, &until); err != nil {
				return err
			}
			if on || from != "08:30" || until != "24:00" {
				t.Errorf("stored notifications %v, hours %s-%s; want off (left out), 08:30-24:00", on, from, until)
			}
			if err := tx.QueryRow(ctx, `SELECT body::jsonb #>> '{payload,changes}' FROM ledger
				WHERE kind = 'business.updated' AND subject_id::text = current_tenant()::text ORDER BY seq DESC LIMIT 1`).Scan(&change); err != nil {
				return err
			}
			for _, want := range []string{`"notifications": {"new": "off", "old": "on"}`, `"notify_hours": {"new": "08:30-24:00", "old": "09:00-19:00"}`} {
				if !strings.Contains(change, want) {
					t.Errorf("business.updated changes = %s; want %s", change, want)
				}
			}
			return nil
		})
		f.Business.Notifications, f.Business.NotifyHours = "on", ""
		if _, err := setup.Import(ctx, owner, app, f); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("sharing: off is stored and recorded (D93)", func(t *testing.T) {
		sharing := func() bool {
			var on bool
			in(func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT sharing FROM tenants WHERE id = current_tenant()`).Scan(&on)
			})
			return on
		}
		if !sharing() {
			t.Fatal("the demo file (no sharing line) stored sharing off")
		}
		f.Business.Sharing = "off"
		if again, err := setup.Import(ctx, owner, app, f); err != nil || again.Changes != 1 || sharing() {
			t.Fatalf("import with sharing: off = %+v, %v, sharing %v; want 1 change and off", again, err, sharing())
		}
		in(func(tx pgx.Tx) error {
			var change string
			if err := tx.QueryRow(ctx, `SELECT body::jsonb #>> '{payload,changes}' FROM ledger
				WHERE kind = 'business.updated' AND subject_id::text = current_tenant()::text ORDER BY seq DESC LIMIT 1`).Scan(&change); err != nil {
				return err
			}
			if !strings.Contains(change, `"sharing": {"new": "off", "old": "on"}`) {
				t.Errorf("business.updated changes = %s; want sharing off", change)
			}
			return nil
		})
		f.Business.Sharing = ""
		if _, err := setup.Import(ctx, owner, app, f); err != nil || !sharing() {
			t.Fatalf("sharing left out again: %v, sharing %v; want on", err, sharing())
		}
	})

	// A writer holds the ledger head and is about to add its event, whose tenant check locks the
	// business's row (as a foreign key check does). An import at that moment waits for the writer,
	// instead of locking that row first and waiting in a circle (a deadlock).
	t.Run("an import waits for a writer, without a deadlock", func(t *testing.T) {
		f.Business.Holidays = []string{"2026-11-09", "2026-11-10"}
		done := make(chan error, 1)
		err := db.WithTenant(ctx, owner, first.TenantID, func(tx pgx.Tx) error {
			if err := ledger.Lock(ctx, tx); err != nil {
				return err
			}
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			go func() {
				again, err := setup.Import(ctx, owner, app, f)
				if err == nil && again.Changes != 1 {
					err = fmt.Errorf("import = %+v; want exactly 1 change", again)
				}
				done <- err
			}()
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				var waiting int
				if err := owner.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))", pid).
					Scan(&waiting); err != nil {
					return err
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					return errors.New("the import never waited for the writer")
				}
			}
			_, err := tx.Exec(ctx, "SELECT 1 FROM tenants WHERE id = current_tenant() FOR KEY SHARE")
			return err
		})
		if err != nil {
			t.Errorf("the writer: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("the import: %v", err)
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
