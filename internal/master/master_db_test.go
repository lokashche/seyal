//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package master_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
)

func TestSaveAndDeactivate(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Master Test")
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	workType := lib.Masters["work_type"]
	by := ledger.PersonActor(person)

	inTx := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, app, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	events := func() (n int) {
		inTx(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE subject_type = 'master_item'").Scan(&n)
		})
		return n
	}
	save := func(name string, fields engine.Fields) (it master.Item, changed bool) {
		inTx(func(tx pgx.Tx) error {
			var err error
			it, changed, err = master.Save(ctx, tx, workType, "monthly-books", name, fields, by)
			return err
		})
		return it, changed
	}

	fields := engine.Fields{"standard_fee": 1200000.0, "is_new": false}
	first, changed := save("Monthly bookkeeping", fields)
	if !changed || first.Version != 1 || !first.Active {
		t.Fatalf("first save = %+v, changed=%v; want version 1, active", first, changed)
	}

	t.Run("saving the same again changes nothing", func(t *testing.T) {
		if _, changed := save("Monthly bookkeeping", fields); changed || events() != 1 {
			t.Errorf("changed=%v, events=%d; want no change and still 1 event", changed, events())
		}
	})

	t.Run("a change bumps the version", func(t *testing.T) {
		it, changed := save("Monthly bookkeeping", engine.Fields{"standard_fee": 1500000.0, "is_new": false})
		if !changed || it.Version != 2 || events() != 2 {
			t.Errorf("save = %+v, changed=%v, events=%d; want version 2 and 2 events", it, changed, events())
		}
	})

	t.Run("deactivate and bring back", func(t *testing.T) {
		inTx(func(tx pgx.Tx) error { return master.Deactivate(ctx, tx, "work_type", "monthly-books", by) })
		inTx(func(tx pgx.Tx) error {
			it, err := master.Get(ctx, tx, "work_type", "monthly-books")
			if err == nil && (it.Active || it.Version != 3) {
				t.Errorf("after deactivate = %+v, want inactive, version 3", it)
			}
			return err
		})
		if it, _ := save("Monthly bookkeeping", engine.Fields{"standard_fee": 1500000.0, "is_new": false}); !it.Active || it.Version != 4 {
			t.Errorf("save after deactivate = %+v, want active again, version 4", it)
		}
	})

	t.Run("bad data is refused", func(t *testing.T) {
		err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
			_, _, err := master.Save(ctx, tx, workType, "monthly-books", "Monthly bookkeeping",
				engine.Fields{"standard_fee": "lots"}, by)
			return err
		})
		if err == nil {
			t.Error("Save accepted text for a money field")
		}
	})

	t.Run("missing item", func(t *testing.T) {
		inTx(func(tx pgx.Tx) error {
			if _, err := master.Get(ctx, tx, "work_type", "no-such"); !errors.Is(err, master.ErrNotFound) {
				t.Errorf("Get missing = %v, want ErrNotFound", err)
			}
			return nil
		})
	})

	t.Run("another business can't see it", func(t *testing.T) {
		other, _ := testdb.NewTenant(t, owner, "Master Test B")
		err := db.WithTenant(ctx, app, other, func(tx pgx.Tx) error {
			_, err := master.Get(ctx, tx, "work_type", "monthly-books")
			return err
		})
		if !errors.Is(err, master.ErrNotFound) {
			t.Errorf("other business Get = %v, want ErrNotFound", err)
		}
	})

	t.Run("ledger is intact", func(t *testing.T) {
		inTx(func(tx pgx.Tx) error {
			rep, err := ledger.Verify(ctx, tx, ledger.Checkpoint{})
			if err == nil && !rep.OK() {
				t.Errorf("ledger problems: %+v", rep.Problems)
			}
			return err
		})
	})
}

// TestLookupFeedsTheEngine checks the whole path: master data saved in the database answers the
// sample playbook's owner rule.
func TestLookupFeedsTheEngine(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Master Test")
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	p := lib.Playbooks["job-evaluation"]
	ownerRule := *p.Approvals.Slots[2].RequiredIf
	by := ledger.PersonActor(person)

	err = db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		for key, isNew := range map[string]bool{"monthly-books": false, "new-service": true} {
			if _, _, err := master.Save(ctx, tx, lib.Masters["work_type"], key, key,
				engine.Fields{"is_new": isNew}, by); err != nil {
				return err
			}
		}

		tests := []struct {
			workType string
			want     engine.Result
		}{
			{"monthly-books", engine.False},
			{"new-service", engine.True},
			{"not-saved", engine.Unknown},
		}
		for _, tt := range tests {
			fields := engine.Fields{"nature_of_work": tt.workType, "fee_agreed": 100.0, "standard_fee": 100.0,
				"integrity_check": "pass"}
			lookup, err := master.LookupFor(ctx, tx, p, fields)
			if err != nil {
				return err
			}
			got, err := engine.Eval(p, ownerRule, fields, lookup)
			if err != nil {
				return err
			}
			if got != tt.want {
				t.Errorf("owner rule with work type %s = %v, want %v", tt.workType, got, tt.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
