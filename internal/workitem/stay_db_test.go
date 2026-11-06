//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// mark gives the item's step stay a reminder and an escalation outside Seyal, as the chaser of part 2
// will: the next change shows whether it keeps them (a pass, a take) or starts a new stay (a move).
func (f *firm) mark(id string) workitem.Item {
	f.t.Helper()
	err := db.WithTenant(f.ctx, f.ownerDB, f.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `UPDATE work_items SET reminded_at = now(), reminders = 1, escalated_at = now()
			WHERE id::text = $1`, id)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		it, err = workitem.Get(f.ctx, tx, id)
		return err
	})
	return it
}

// newStay checks that it is in a new step stay, started after before's: step_since later, and no
// reminders or escalation carried over.
func newStay(t *testing.T, what string, before, it workitem.Item) {
	t.Helper()
	if it.StepSince == "" || it.StepSince <= before.StepSince {
		t.Errorf("%s: in this step since %q (before %q); want a new step stay", what, it.StepSince, before.StepSince)
	}
	if it.RemindedAt != "" || it.Reminders != 0 || it.EscalatedAt != "" {
		t.Errorf("%s: reminded %q (%d), escalated %q; want them cleared in the new step stay",
			what, it.RemindedAt, it.Reminders, it.EscalatedAt)
	}
}

// sameStay checks that it is still in before's step stay, with what the chaser did there kept.
func sameStay(t *testing.T, what string, before, it workitem.Item) {
	t.Helper()
	if it.StepSince != before.StepSince || it.RemindedAt != before.RemindedAt || it.Reminders != before.Reminders ||
		it.EscalatedAt != before.EscalatedAt {
		t.Errorf("%s: stay %q/%q/%d/%q, want it unchanged: %q/%q/%d/%q", what, it.StepSince, it.RemindedAt,
			it.Reminders, it.EscalatedAt, before.StepSince, before.RemindedAt, before.Reminders, before.EscalatedAt)
	}
}

// TestStepStays follows sample job evaluation 0.5 (D76): every way into a step starts a new step stay
// (start, a move, a send back, approvals settling, an edit that reopens approvals, a refusal), and a
// take or a pass inside the step keeps it, with its reminders and escalation. `seyal verify` stays clean.
func TestStepStays(t *testing.T) {
	f := newHandFirm(t)
	job := f.start(ready())
	if job.State != "draft" || job.StepSince == "" || job.RemindedAt != "" || job.Reminders != 0 || job.EscalatedAt != "" {
		t.Fatalf("new item: %s, stay %q/%q/%d/%q; want draft, in this step since now and nothing else",
			job.State, job.StepSince, job.RemindedAt, job.Reminders, job.EscalatedAt)
	}
	if got := f.get(job.ID); got.StepSince != job.StepSince {
		t.Errorf("stored step_since %q, the start returned %q", got.StepSince, job.StepSince)
	}

	before := f.mark(job.ID)
	job = f.act(job.ID, "send_to_ops", "")
	newStay(t, "sent to Ops (a move)", before, job)

	before = f.mark(job.ID)
	job, err := f.take(job.ID, "team:ops", f.dev)
	if err != nil {
		t.Fatal(err)
	}
	sameStay(t, "taken from the team", before, job)
	job, _, err = f.pass(job.ID, workitem.PassOn{Person: f.ops, Via: "panel"}, f.dev)
	if err != nil {
		t.Fatal(err)
	}
	sameStay(t, "passed on", before, job)
	job, _, err = f.pass(job.ID, workitem.PassOn{Person: f.dev, Via: "panel"}, f.ops)
	if err != nil {
		t.Fatal(err)
	}
	sameStay(t, "passed back", before, job)

	job, err = f.actAs(job.ID, "back_to_sales", "a statement is missing", ledger.PersonActor(f.dev))
	if err != nil || job.State != "draft" {
		t.Fatalf("send back = %v, %s", err, job.State)
	}
	newStay(t, "sent back", before, job)

	before = f.mark(job.ID)
	job = f.act(job.ID, "send_to_ops", "")
	newStay(t, "sent to Ops again (returns to Dev)", before, job)

	before = job
	if job, err = f.actAs(job.ID, "submit", "", ledger.PersonActor(f.dev)); err != nil || job.State != "pending_approval" {
		t.Fatalf("submit = %v, %s", err, job.State)
	}
	newStay(t, "submitted", before, job)

	before = f.mark(job.ID)
	f.decide(job.ID, f.dev, "ops", engine.Approve, "")
	job = f.decide(job.ID, f.owner, "owner", engine.Approve, "")
	if job.State != "approved" {
		t.Fatalf("after both approvals: %s, want approved", job.State)
	}
	newStay(t, "approvals settled", before, job)

	before = f.mark(job.ID)
	job, err = f.setAs(job.ID, engine.Fields{"reason": "Monthly books, from April"}, f.admin)
	if err != nil || job.State != "pending_approval" {
		t.Fatalf("edit after approval = %v, %s; want back to pending approval", err, job.State)
	}
	newStay(t, "an edit that reopens approvals", before, job)

	before = job
	job = f.refuse(job.ID, f.owner, "owner", "The fee is too low")
	if job.State == "pending_approval" {
		t.Fatalf("after a refusal: still %s", job.State)
	}
	newStay(t, "refused (approvals settled the other way)", before, job)
	f.checkTables()
}

// TestStepStaysOfLinkedWork: a step's actions that start linked work give it its own step stay, and
// linked work moving its parent on starts a new stay for the parent.
func TestStepStaysOfLinkedWork(t *testing.T) {
	f := newFirm(t)
	by := ledger.PersonActor(f.sales)
	var pv string
	f.tx(func(tx pgx.Tx) error {
		for _, src := range []string{waitCheck, waitParent} {
			p, err := playbook.Parse([]byte(src))
			if err == nil {
				err = p.Validate()
			}
			if err != nil {
				return err
			}
			if pv, err = playbook.EnsureUsed(f.ctx, tx, p, by); err != nil {
				return err
			}
		}
		return nil
	})
	var parent, child workitem.Item
	var err error
	f.tx(func(tx pgx.Tx) error {
		parent, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by,
			Fields: engine.Fields{"topic": "Year-end"}})
		return err
	})
	before := f.mark(parent.ID)
	parent = f.act(parent.ID, "go", "")
	newStay(t, "parent waiting (its step starts linked work)", before, parent)

	var childID string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT to_item_id::text FROM work_item_links WHERE from_item_id = $1 AND kind = 'parent'`,
			parent.ID).Scan(&childID)
	})
	f.tx(func(tx pgx.Tx) error { child, err = workitem.Get(f.ctx, tx, childID); return err })
	if child.StepSince == "" {
		t.Error("the linked work has no step stay")
	}

	before = f.mark(parent.ID)
	f.tx(func(tx pgx.Tx) error {
		_, err := workitem.Act(f.ctx, tx, childID, "finish", "", ledger.SystemActor)
		return err
	})
	f.tx(func(tx pgx.Tx) error { parent, err = workitem.Get(f.ctx, tx, parent.ID); return err })
	if parent.State != "back" {
		t.Fatalf("parent = %s, want back", parent.State)
	}
	newStay(t, "parent moved on by its linked work", before, parent)
	f.checkTables()
}
