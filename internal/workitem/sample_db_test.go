//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// TestSampleJobEvaluation follows the shipped sample (examples/, version 0.2): Sales sends the job to
// the Ops team, Ops confirms what's needed, and the approvals follow the fee rules.
func TestSampleJobEvaluation(t *testing.T) {
	f := newFirm(t)
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	by := ledger.PersonActor(f.sales)
	f.tx(func(tx pgx.Tx) error {
		if pv, _, err = playbook.Register(f.ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		for _, w := range []struct {
			key   string
			fixed bool
		}{{"gst", true}, {"books", false}} {
			if _, _, err := master.Save(f.ctx, tx, lib.Masters["work_type"], w.key, w.key, engine.Fields{
				"standard_fee": 500000.0, "documents": []any{"Register"}, "timeline_days": 5.0, "maker": f.maker,
				"is_new": false, "fixed_price": w.fixed}, by); err != nil {
				return err
			}
		}
		return nil
	})
	act := func(id, action string, who string) workitem.Item {
		t.Helper()
		var it workitem.Item
		f.tx(func(tx pgx.Tx) error {
			it, err = workitem.Act(f.ctx, tx, id, action, "", ledger.PersonActor(who))
			return err
		})
		return it
	}
	set := func(id string, v engine.Fields, who string) workitem.Item {
		t.Helper()
		var it workitem.Item
		f.tx(func(tx pgx.Tx) error {
			it, err = workitem.SetFields(f.ctx, tx, id, v, ledger.PersonActor(who))
			return err
		})
		return it
	}
	waiting := func(id string) []string {
		t.Helper()
		var st engine.ApprovalStatus
		f.tx(func(tx pgx.Tx) error {
			st, err = workitem.Approvals(f.ctx, tx, id)
			return err
		})
		return st.Waiting
	}
	start := func(work string, fee any) workitem.Item {
		t.Helper()
		fields := engine.Fields{"client": "acme", "nature_of_work": work, "reason": "Books", "integrity_check": "pass"}
		if fee != nil {
			fields["fee_agreed"] = fee
		}
		var it workitem.Item
		f.tx(func(tx pgx.Tx) error {
			it, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", Fields: fields, By: by})
			return err
		})
		return it
	}

	// Fixed price, fee as proposed and not below standard: only Ops approves.
	job := start("gst", 500000.0)
	if job.AssigneeID != f.sales {
		t.Errorf("a new job is with %q, want Sales who started it", job.AssigneeID)
	}
	job = act(job.ID, "send_to_ops", f.sales)
	if job.State != "with_ops" || job.Team != "ops" || job.Fields["fee_proposed"] != 500000.0 {
		t.Fatalf("after send_to_ops: %s, team %q, proposed %v", job.State, job.Team, job.Fields["fee_proposed"])
	}
	job = act(job.ID, "submit", f.ops) // documents, timeline and maker came from the work type
	if got := waiting(job.ID); len(got) != 1 || got[0] != "ops" {
		t.Errorf("waiting for %v, want only ops", got)
	}

	// Ops changes the fee: Sales must confirm again.
	job2 := act(start("gst", 500000.0).ID, "send_to_ops", f.sales)
	set(job2.ID, engine.Fields{"fee_agreed": 600000.0}, f.ops)
	job2 = act(job2.ID, "submit", f.ops)
	if got := waiting(job2.ID); len(got) != 2 || got[0] != "ops" || got[1] != "sales" {
		t.Errorf("fee changed by Ops: waiting for %v, want ops and sales", got)
	}

	// Variable price: the Owner always approves; Sales gave no fee, so Ops must confirm one first and
	// Sales confirms it.
	job3 := act(start("books", nil).ID, "send_to_ops", f.sales)
	if err := f.try(func(tx pgx.Tx) error {
		_, err := workitem.Act(f.ctx, tx, job3.ID, "submit", "", ledger.PersonActor(f.ops))
		return err
	}); err == nil {
		t.Error("submitted without a confirmed fee")
	}
	set(job3.ID, engine.Fields{"fee_agreed": 700000.0}, f.ops)
	job3 = act(job3.ID, "submit", f.ops)
	if got := waiting(job3.ID); len(got) != 3 {
		t.Errorf("variable price, no fee from Sales: waiting for %v, want ops, sales and owner", got)
	}

	f.tx(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}
