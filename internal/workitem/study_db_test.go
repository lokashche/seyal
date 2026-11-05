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

// TestSampleRequirementStudy follows the shipped samples: a job for a work type that is new for the
// team starts a linked Requirement study; the job waits (and nobody can move it on by hand); when the
// study is done, its findings fill the job and the job goes to the Ops team.
func TestSampleRequirementStudy(t *testing.T) {
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
	act := func(id, action string, who ledger.Actor) (workitem.Item, error) {
		var it workitem.Item
		err := f.try(func(tx pgx.Tx) error {
			var err error
			it, err = workitem.Act(f.ctx, tx, id, action, "", who)
			return err
		})
		return it, err
	}
	get := func(id string) workitem.Item {
		var it workitem.Item
		f.tx(func(tx pgx.Tx) error { it, err = workitem.Get(f.ctx, tx, id); return err })
		return it
	}

	var job workitem.Item
	f.tx(func(tx pgx.Tx) error {
		job, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by,
			Fields: engine.Fields{"client": "acme", "nature_of_work": "payroll", "reason": "New payroll", "integrity_check": "pass"}})
		return err
	})
	if job, err = act(job.ID, "send_to_ops", by); err != nil || job.State != "studying" {
		t.Fatalf("send_to_ops of new work = %v, %s; want studying", err, job.State)
	}

	var studyID string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT to_item_id::text FROM work_item_links WHERE from_item_id = $1 AND kind = 'parent'`,
			job.ID).Scan(&studyID)
	})
	study := get(studyID)
	if study.State != "studying" || study.Team != "ops" || study.Fields["client"] != "acme" ||
		study.Fields["nature_of_work"] != "payroll" || study.Number == "" {
		t.Fatalf("study = %+v", study)
	}
	if job.AssigneeID != f.sales {
		t.Errorf("the waiting job is with %q, want Sales still", job.AssigneeID)
	}
	if _, err := act(job.ID, "study_done", ledger.PersonActor(f.ops)); err == nil {
		t.Error("a person moved the job on before the study was done")
	}

	ops := ledger.PersonActor(f.ops)
	f.tx(func(tx pgx.Tx) error {
		_, err := workitem.SetFields(f.ctx, tx, studyID, engine.Fields{"approach": "Monthly inputs, then a run",
			"days_needed": 6.0, "people": []any{f.maker, f.ops}}, ops)
		return err
	})
	if study, err = act(studyID, "done", ops); err != nil || study.State != "done" {
		t.Fatalf("study done = %v, %s", err, study.State)
	}
	job = get(job.ID)
	if job.State != "with_ops" || job.Team != "ops" || job.Fields["timeline_days"] != 6.0 {
		t.Errorf("job after the study: %s, team %q, timeline %v; want with_ops, ops team, 6 days",
			job.State, job.Team, job.Fields["timeline_days"])
	}
	if people, _ := job.Fields["resources"].([]any); len(people) != 2 {
		t.Errorf("resources = %v, want the study's two people", job.Fields["resources"])
	}
	// Seyal fills the job's read-only "Study findings" and "Work type studied" (sample 0.5).
	if job.Fields["study_findings"] != "Monthly inputs, then a run" {
		t.Errorf("study_findings = %v, want the study's approach", job.Fields["study_findings"])
	}
	f.tx(func(tx pgx.Tx) error {
		_, err := workitem.SetFields(f.ctx, tx, job.ID, engine.Fields{"study_findings": "changed"}, ops)
		if err == nil {
			t.Error("a person changed the read-only study findings")
		}
		return nil
	})

	// A job sent again for the same work type isn't studied a second time; a job sent again for a
	// different work type that is also new is studied again (D68, choice 9).
	studies := func() int {
		var n int
		f.tx(func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, `SELECT count(*) FROM work_item_links WHERE from_item_id = $1 AND kind = 'parent'`,
				job.ID).Scan(&n)
		})
		return n
	}
	backToSales := func() {
		t.Helper()
		f.tx(func(tx pgx.Tx) error {
			job, err = workitem.Act(f.ctx, tx, job.ID, "back_to_sales", "please check the reason", ops)
			return err
		})
	}
	if job.Fields["studied_work_type"] != "payroll" {
		t.Errorf("studied_work_type = %v, want payroll", job.Fields["studied_work_type"])
	}
	backToSales()
	if job, err = act(job.ID, "send_to_ops", by); err != nil || job.State != "with_ops" || studies() != 1 {
		t.Errorf("sent again for the same work type = %v, %s, %d studies; want with_ops and still one study",
			err, job.State, studies())
	}
	backToSales()
	f.tx(func(tx pgx.Tx) error {
		if _, _, err := master.Save(f.ctx, tx, lib.Masters["work_type"], "pf-returns", "PF returns", engine.Fields{
			"standard_fee": 300000.0, "is_new": true, "fixed_price": false}, by); err != nil {
			return err
		}
		_, err := workitem.SetFields(f.ctx, tx, job.ID, engine.Fields{"nature_of_work": "pf-returns"}, by)
		return err
	})
	if job, err = act(job.ID, "send_to_ops", by); err != nil || job.State != "studying" || studies() != 2 ||
		job.Fields["studied_work_type"] != "pf-returns" {
		t.Errorf("sent again for another new work type = %v, %s, %d studies, studied %v; want studying, a second study, pf-returns",
			err, job.State, studies(), job.Fields["studied_work_type"])
	}

	f.tx(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}

// A made-up parent that waits for linked work, and the linked work, which reports back one field the
// parent has and one it doesn't (as Requirement study 0.2 does to a job still on Job evaluation 0.4).
const (
	waitParent = `id: wait-parent
version: "1"
description: Waits for a made-up check.
numbering: "P-{seq:4}"
fields:
  - { key: topic, label: Topic, type: text, required: true }
  - { key: answer, label: Answer, type: text, readonly: true }
states: [new, waiting, back, closed]
initial: new
transitions:
  - { action: go, from: [new], to: waiting }
  - { action: check_done, from: [waiting], to: back, by: seyal }
  - { action: close, from: [new, waiting, back], to: closed, needs: reason }
on:
  waiting:
    - { do: start, process: wait-check, copy: { topic: topic } }
`
	waitCheck = `id: wait-check
version: "1"
description: A made-up check that reports back.
numbering: "C-{seq:4}"
fields:
  - { key: topic, label: Topic, type: text }
  - { key: result, label: Result, type: text }
states: [open, done]
initial: open
transitions:
  - { action: finish, from: [open], to: done }
on:
  done:
    - { do: tell_parent, action: check_done, copy: { answer: result, findings: result } }
`
)

// TestReportToAnOlderParent: linked work that reports a field its parent's version doesn't have
// still finishes; the fields the parent has are filled and the parent moves on (D69).
func TestReportToAnOlderParent(t *testing.T) {
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
	var parent workitem.Item
	var err error
	f.tx(func(tx pgx.Tx) error {
		if parent, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by,
			Fields: engine.Fields{"topic": "Year-end"}}); err != nil {
			return err
		}
		parent, err = workitem.Act(f.ctx, tx, parent.ID, "go", "", by)
		return err
	})
	var childID string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT to_item_id::text FROM work_item_links WHERE from_item_id = $1 AND kind = 'parent'`,
			parent.ID).Scan(&childID)
	})
	err = f.try(func(tx pgx.Tx) error {
		if _, err := workitem.SetFields(f.ctx, tx, childID, engine.Fields{"result": "All fine"}, ledger.SystemActor); err != nil {
			return err
		}
		_, err := workitem.Act(f.ctx, tx, childID, "finish", "", ledger.SystemActor)
		return err
	})
	if err != nil {
		t.Fatalf("finishing the linked work = %v, want it to finish", err)
	}
	f.tx(func(tx pgx.Tx) error { parent, err = workitem.Get(f.ctx, tx, parent.ID); return err })
	if parent.State != "back" || parent.Fields["answer"] != "All fine" || parent.Fields["findings"] != nil {
		t.Errorf("parent = %s, %v; want back, with the answer and nothing else", parent.State, parent.Fields)
	}
	f.checkTables()
}
