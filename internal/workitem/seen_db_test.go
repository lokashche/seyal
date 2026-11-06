//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/workitem"
)

// TestItemSeen (D75, M7): a seen time is kept per person and item, and a newer one is never replaced by
// an older one. item_seen is locked like every business table: another business sees none of its rows
// and can't write one for this business's person or item, nothing is seen without a business, and the
// app may not delete rows.
func TestItemSeen(t *testing.T) {
	f := newFirm(t)
	g := newFirm(t)
	it := f.start(ready())
	other := g.start(ready())
	first := time.Date(2026, 12, 2, 4, 10, 0, 123456000, time.UTC)

	seen := func(f *firm, person string) map[string]string {
		t.Helper()
		var got map[string]string
		f.tx(func(tx pgx.Tx) error {
			var err error
			got, err = workitem.SeenBy(f.ctx, tx, person)
			return err
		})
		return got
	}
	f.tx(func(tx pgx.Tx) error { return workitem.MarkSeen(f.ctx, tx, f.ops, it.ID, first) })
	f.tx(func(tx pgx.Tx) error { return workitem.MarkSeen(f.ctx, tx, f.ops, it.ID, first.Add(-time.Hour)) })
	if got := seen(f, f.ops)[it.ID]; got != "2026-12-02T04:10:00.123456Z" {
		t.Errorf("seen at %q, want the newer time in the snapshot's layout", got)
	}
	if got := seen(f, f.sales); len(got) != 0 {
		t.Errorf("another person's seen times = %v, want none (each person has their own)", got)
	}

	t.Run("another business sees nothing and can't write here", func(t *testing.T) {
		if got := seen(g, f.ops); len(got) != 0 {
			t.Errorf("business 2 sees %v", got)
		}
		for name, args := range map[string][]any{
			"this business's row":        {f.tenant, f.ops, it.ID},
			"its own row, our item":      {g.tenant, g.ops, it.ID},
			"its own row, our person":    {g.tenant, f.ops, other.ID},
			"its own row, its own thing": {g.tenant, g.ops, other.ID}, // the one that works
		} {
			err := g.try(func(tx pgx.Tx) error {
				_, err := tx.Exec(g.ctx, `INSERT INTO item_seen (tenant_id, person_id, item_id, seen_at) VALUES ($1, $2, $3, now())`, args...)
				return err
			})
			if ok := name == "its own row, its own thing"; (err == nil) != ok {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
	t.Run("no business: nothing", func(t *testing.T) {
		var n int
		err := db.WithServer(f.ctx, f.app, func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, "SELECT count(*) FROM item_seen").Scan(&n)
		})
		if err != nil || n != 0 {
			t.Errorf("without a business: %d rows (%v), want 0", n, err)
		}
	})
	t.Run("the app can't delete", func(t *testing.T) {
		err := f.try(func(tx pgx.Tx) error {
			_, err := tx.Exec(f.ctx, "DELETE FROM item_seen")
			return err
		})
		if err == nil {
			t.Error("the app deleted seen times")
		}
	})
}
