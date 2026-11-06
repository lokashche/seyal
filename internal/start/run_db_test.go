//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package start

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/setup"
	"seyal/internal/testdb"
)

// TestSecondStartIsRefusedQuietly: the unique started_ref stops a second start of the same work even
// when the check before it was fooled (here: a plan read before the first start, as a second copy of
// the app could have). Only the savepoint is undone, so the transaction goes on, and one item is left.
func TestSecondStartIsRefusedQuietly(t *testing.T) {
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
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10])
	for i, p := range f.People {
		f.People[i].Email = p.Key + "@" + f.Business.Slug + ".kaveri-demo.example"
	}
	sum, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	zone, _ := time.LoadLocation("Asia/Kolkata")
	sched := playbook.Schedule{Every: "month", Day: 1, Period: "previous"}
	runDay := Next(sched, Due(sched, time.Now().In(zone), zone, 4), 4).RunDay // the next 1st: after the demo came in
	today := time.Date(runDay.Year(), runDay.Month(), runDay.Day(), 0, 0, 0, 0, zone)

	err = db.WithTenant(ctx, app, sum.TenantID, func(tx pgx.Tx) error {
		if err := ledger.Lock(ctx, tx); err != nil {
			return err
		}
		p, ok, err := For(ctx, tx, "monthly-return", today)
		if err != nil || !ok || !p.Ready {
			t.Fatalf("plan: %+v, %v, %v", p, ok, err)
		}
		r := Request{PlaybookID: p.PlaybookID, Period: p.Period.Key, Today: today}
		if did, err := Run(ctx, tx, r); err != nil || !strings.Contains(did, "run covers 2") {
			t.Fatalf("decide: %q, %v", did, err)
		}
		stale, _, err := For(ctx, tx, "monthly-return", today) // decided, nothing started yet
		if err != nil {
			return err
		}
		r.Entry = "sundaram-textiles"
		if did, err := startOne(ctx, tx, stale, r); err != nil || !strings.HasPrefix(did, "started M-") {
			t.Fatalf("first start: %q, %v", did, err)
		}
		if did, err := startOne(ctx, tx, stale, r); err != nil || did != "" {
			t.Errorf("second start with the stale plan: %q, %v; want it refused quietly", did, err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE started_ref = $1`, stale.Ref("sundaram-textiles")).Scan(&n); err != nil {
			return err // the transaction must still work after the refused insert
		}
		if n != 1 {
			t.Errorf("%d items for one entry, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
