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

	f.tx(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}
