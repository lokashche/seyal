//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/workitem"
)

func (f *handFirm) reassign(ids []string, from, to, note, by string) (workitem.Reassigned, error) {
	var done workitem.Reassigned
	err := f.try(func(tx pgx.Tx) error {
		var err error
		done, err = workitem.Reassign(f.ctx, tx, ids, from, to, note, ledger.PersonActor(by))
		return err
	})
	return done, err
}

// TestReassign moves Olga's open work to Dev in one press (D75, M3): only an admin may, and only while
// still an admin when pressing; the receiver must be able to receive work; an item that changed hands
// since the list was shown is skipped and named while the others still move; every move of one press
// shares one batch id; the event names who pressed (from_assignee), who had it (from_holder), how
// (reassign), as an admin, by button; an empty note becomes "Moved here from Olga Ops"; in a step owned
// by a person field the field follows; and `seyal verify` stays clean.
func TestReassign(t *testing.T) {
	f := newHandFirm(t)
	job1 := f.act(f.start(ready()).ID, "send_to_ops", "")
	job2 := f.act(f.start(ready()).ID, "send_to_ops", "")
	for _, id := range []string{job1.ID, job2.ID} {
		if _, err := f.take(id, "team:ops", f.ops); err != nil {
			t.Fatal(err)
		}
	}
	var other workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		if other, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: f.other, Via: "chat",
			Fields: engine.Fields{"what": "Renew the office internet plan"}, By: ledger.PersonActor(f.sales)}); err != nil {
			return err
		}
		other, err = workitem.SetFields(f.ctx, tx, other.ID, engine.Fields{"who": f.ops}, ledger.PersonActor(f.sales))
		return err
	})
	if other.AssigneeID != f.ops {
		t.Fatalf("Other work is with %q, want Olga", other.AssigneeID)
	}
	ids := []string{job1.ID, job2.ID, other.ID}
	var work []workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		work, err = workitem.OpenWorkOf(f.ctx, tx, f.ops)
		return err
	})
	if len(work) != 3 || work[0].Number > work[1].Number {
		t.Fatalf("Olga's open work = %d items, want 3 by number", len(work))
	}

	stillWithOlga := func(what string) {
		t.Helper()
		for _, id := range ids {
			if it := f.get(id); it.AssigneeID != f.ops {
				t.Errorf("%s: %s is with %q, want it still with Olga", what, it.Number, it.AssigneeID)
			}
		}
	}

	// Olga herself, or anyone else who isn't an admin, is refused: nothing moves.
	if _, err := f.reassign(ids, f.ops, f.dev, "", f.ops); !errors.Is(err, workitem.ErrNotAdmin) {
		t.Errorf("a non-admin's move = %v; want ErrNotAdmin", err)
	}
	// The receiver must be able to receive work: not Olga again, not Mia (no login).
	_, err := f.reassign(ids, f.ops, f.ops, "", f.admin)
	wantRefused(t, "moving to the same person", err, "choose someone else")
	_, err = f.reassign(ids, f.ops, f.maker, "", f.admin)
	wantRefused(t, "moving to someone with no login", err, "hasn't joined yet")
	// Chitra loses her admin rights after the list was shown and before she presses: nothing moves.
	f.tx(func(tx pgx.Tx) error {
		_, err := people.Revoke(f.ctx, tx, f.admin, workitem.AdminRole, ledger.SystemActor)
		return err
	})
	if _, err := f.reassign(ids, f.ops, f.dev, "", f.admin); !errors.Is(err, workitem.ErrNotAdmin) {
		t.Errorf("a move by a former admin = %v; want ErrNotAdmin", err)
	}
	stillWithOlga("after the refusals")
	if n := len(f.events(job1.ID, "item.assigned")); n != 3 { // the starter, the Ops team, Olga's take
		t.Errorf("the refusals recorded hand-overs: %d, want 3", n)
	}
	f.tx(func(tx pgx.Tx) error {
		_, err := people.Grant(f.ctx, tx, f.admin, workitem.AdminRole, ledger.SystemActor)
		return err
	})

	// Meanwhile Olga gives job 2 back to the Ops team: it is skipped and named, the others move.
	if _, _, err := f.pass(job2.ID, workitem.PassOn{Team: "ops"}, f.ops); err != nil {
		t.Fatal(err)
	}
	done, err := f.reassign(ids, f.ops, f.dev, "  ", f.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Moved) != 2 || len(done.Skipped) != 1 || done.Skipped[0].Number != job2.Number ||
		done.Skipped[0].Why != "it is with the ops team now" || done.Batch == "" {
		t.Errorf("the press = %+v; want 2 moved, and %s skipped as now with the Ops team", done, job2.Number)
	}
	if it := f.get(job2.ID); it.Team != "ops" || it.AssigneeID != "" {
		t.Errorf("job 2 is with %q/%q, want the Ops team still", it.AssigneeID, it.Team)
	}
	for _, id := range []string{job1.ID, other.ID} {
		it := f.get(id)
		h := f.lastHandOver(id)
		if it.AssigneeID != f.dev || it.HandoffFrom != f.admin || it.HandoffNote != "Moved here from Olga Ops" {
			t.Errorf("%s: with %q from %q, note %q; want Dev, from Chitra, with the fixed note", it.Number, it.AssigneeID,
				it.HandoffFrom, it.HandoffNote)
		}
		for key, want := range map[string]any{"how": workitem.HowReassign, "as": "admin", "via": "button",
			"from_assignee": f.admin, "from_holder": f.ops, "batch": done.Batch, "note": "Moved here from Olga Ops"} {
			if h.Change[key] != want {
				t.Errorf("%s: the event's %s = %v, want %v", it.Number, key, h.Change[key], want)
			}
		}
		if h.ActorKind != "human" || h.Actor != f.admin {
			t.Errorf("%s: recorded as %s %s, want Chitra (human)", it.Number, h.ActorKind, h.Actor)
		}
	}
	if it := f.get(other.ID); it.Fields["who"] != f.dev || it.Version != other.Version+1 {
		t.Errorf("Other work: who = %v, version %d (was %d); want Dev, one version up (its step belongs to that field)",
			it.Fields["who"], it.Version, other.Version)
	}
	if it := f.get(job1.ID); it.Version != job1.Version {
		t.Errorf("the move changed job 1's version from %d to %d; a hand-over keeps it, so approvals stay", job1.Version, it.Version)
	}

	// A second press has its own batch id; the first press's id is on exactly its two moves.
	if _, err := f.take(job2.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	again, err := f.reassign([]string{job2.ID}, f.ops, f.dev, "Dev knows them", f.admin)
	if err != nil || len(again.Moved) != 1 || again.Batch == done.Batch {
		t.Fatalf("second press = %+v, %v; want one moved, with a new batch id", again, err)
	}
	var batches []string
	f.tx(func(tx pgx.Tx) error {
		rows, _ := tx.Query(f.ctx, `SELECT body::jsonb #>> '{payload,change,batch}' FROM ledger
			WHERE tenant_id = current_tenant() AND kind = 'item.assigned' AND body::jsonb #>> '{payload,change,how}' = 'reassign'
			ORDER BY seq`)
		var err error
		batches, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if !slices.Equal(batches, []string{done.Batch, done.Batch, again.Batch}) {
		t.Errorf("batch ids = %v; want the first press's twice, then the second's", batches)
	}
	if h := f.lastHandOver(job2.ID); h.Change["note"] != "Dev knows them" {
		t.Errorf("a typed note = %v, want it kept", h.Change["note"])
	}

	// An id named twice moves once and isn't named as skipped. A list longer than MaxMove (a forged
	// form) is refused before the lock is taken, and nothing moves.
	twice, err := f.reassign([]string{job2.ID, job2.ID}, f.dev, f.ops, "", f.admin)
	if err != nil || len(twice.Moved) != 1 || len(twice.Skipped) != 0 || len(f.events(job2.ID, "item.assigned")) != 7 {
		t.Errorf("an id named twice = %+v, %v; want it moved once, nothing skipped", twice, err)
	}
	huge := []string{job1.ID}
	for i := range workitem.MaxMove {
		huge = append(huge, fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
	}
	if _, err := f.reassign(huge, f.dev, f.ops, "", f.admin); !errors.Is(err, workitem.ErrTooMany) {
		t.Errorf("a press of %d items = %v; want ErrTooMany", len(huge), err)
	}
	if it := f.get(job1.ID); it.AssigneeID != f.dev {
		t.Errorf("after the refused huge press job 1 is with %q, want Dev still", it.AssigneeID)
	}
	f.checkTables()
}
