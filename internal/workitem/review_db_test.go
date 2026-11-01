//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// TestDecisionOnWhatWasSeen: an approval made from a page showing an older version is refused, so
// nobody approves details they haven't seen.
func TestDecisionOnWhatWasSeen(t *testing.T) {
	f := newFirm(t)
	it := f.start(ready())
	it = f.act(it.ID, "submit", "")
	seen := it.Version
	it = f.set(it.ID, engine.Fields{"fee_agreed": 1000000.0}) // someone changes the fee meanwhile
	err := f.try(func(tx pgx.Tx) error {
		_, err := workitem.DecideSeen(f.ctx, tx, it.ID, seen, f.ops, "ops", engine.Approve, "")
		return err
	})
	if !errors.Is(err, workitem.ErrChanged) {
		t.Fatalf("approving version %d after a change = %v, want ErrChanged", seen, err)
	}
	f.tx(func(tx pgx.Tx) error {
		_, err := workitem.DecideSeen(f.ctx, tx, it.ID, it.Version, f.ops, "ops", engine.Approve, "")
		return err
	})
}

// TestChildFinishesAfterParentCancelled: linked work can still be finished after the item that
// started it was cancelled; it just says nothing could be passed back.
func TestChildFinishesAfterParentCancelled(t *testing.T) {
	f := newFirm(t)
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	by := ledger.PersonActor(f.sales)
	var pv string
	f.tx(func(tx pgx.Tx) error {
		if err := playbook.EnsureStarted(f.ctx, tx, lib, lib.Playbooks["job-evaluation"], by); err != nil {
			return err
		}
		if pv, err = playbook.EnsureUsed(f.ctx, tx, lib.Playbooks["job-evaluation"], by); err != nil {
			return err
		}
		_, _, err := master.Save(f.ctx, tx, lib.Masters["work_type"], "payroll", "Payroll", engine.Fields{
			"standard_fee": 800000.0, "is_new": true, "fixed_price": false}, by)
		return err
	})
	var job workitem.Item
	f.tx(func(tx pgx.Tx) error {
		job, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by,
			Fields: engine.Fields{"client": "acme", "nature_of_work": "payroll", "reason": "New payroll", "integrity_check": "pass"}})
		return err
	})
	job = f.act(job.ID, "send_to_ops", "")
	var studyID string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT to_item_id::text FROM work_item_links WHERE from_item_id = $1 AND kind = 'parent'`,
			job.ID).Scan(&studyID)
	})
	f.act(job.ID, "cancel", "the client changed their mind")

	ops := ledger.PersonActor(f.ops)
	var study workitem.Item
	f.tx(func(tx pgx.Tx) error {
		if _, err := workitem.SetFields(f.ctx, tx, studyID, engine.Fields{"approach": "Monthly run",
			"days_needed": 6.0, "people": []any{f.ops}}, ops); err != nil {
			return err
		}
		study, err = workitem.Act(f.ctx, tx, studyID, "done", "", ops)
		return err
	})
	if study.State != "done" {
		t.Errorf("study = %s, want done although its job was cancelled", study.State)
	}
}
