//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// handOver is one item.assigned event: who recorded it and what changed.
type handOver struct {
	ActorKind, Actor string
	Change           map[string]any
}

// lastHandOver reads the item's latest item.assigned event.
func (f *firm) lastHandOver(id string) handOver {
	f.t.Helper()
	var h handOver
	f.tx(func(tx pgx.Tx) error {
		var body string
		err := tx.QueryRow(f.ctx, `
			SELECT actor_kind, coalesce(actor_person_id::text, ''), body FROM ledger
			WHERE subject_id = $1::uuid AND kind = 'item.assigned' ORDER BY seq DESC LIMIT 1`, id).
			Scan(&h.ActorKind, &h.Actor, &body)
		if err != nil {
			return err
		}
		var e struct {
			Payload struct {
				Change map[string]any `json:"change"`
			} `json:"payload"`
		}
		err = json.Unmarshal([]byte(body), &e)
		h.Change = e.Payload.Change
		return err
	})
	return h
}

func (f *firm) hand(id, person, team string, by ledger.Actor, how, note, from, after string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.HandForTest(f.ctx, tx, id, person, team, by, how, note, from, after)
		return err
	})
	return it
}

// TestHandOvers follows one job on sample job evaluation 0.5 through each kind of hand-over (D70): the
// step's owner (the Ops team), a take, a pass with a note, and a return. Each one is recorded by the
// right actor with its keys, sets the three hand-over columns, keeps the version (so an approval
// already given stays), and leaves `seyal verify` clean.
func TestHandOvers(t *testing.T) {
	f := newFirm(t)
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	var dev string
	f.tx(func(tx pgx.Tx) error {
		if f.pv, _, err = playbook.Register(f.ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		if dev, err = people.Add(f.ctx, tx, "Dev Ops", "dev@acme.example", ledger.SystemActor); err != nil {
			return err
		}
		_, err = people.Grant(f.ctx, tx, dev, "ops", ledger.SystemActor)
		return err
	})
	olga := ledger.PersonActor(f.ops)

	started := time.Now().Add(-time.Second)
	job := f.start(ready())
	if job.AssigneeID != f.sales || job.HeldSince == "" || job.HandoffFrom != "" {
		t.Fatalf("a new job: with %q since %q from %q; want with Sales, a time, from nobody",
			job.AssigneeID, job.HeldSince, job.HandoffFrom)
	}
	job = f.act(job.ID, "send_to_ops", "")
	h := f.lastHandOver(job.ID)
	if job.Team != "ops" || job.HandoffFrom != f.sales || h.ActorKind != "system" ||
		h.Change["how"] != workitem.HowStep || h.Change["from_assignee"] != f.sales {
		t.Errorf("sent to Ops: team %q from %q, event %+v; want the Ops team from Sales, by Seyal, how step", job.Team, job.HandoffFrom, h)
	}
	since, err := time.Parse(workitem.HeldSinceLayout, job.HeldSince)
	if err != nil || since.Before(started) || since.After(time.Now()) || since.Nanosecond()%1000 != 0 {
		t.Errorf("held since %q (%v): want just now, in UTC, to whole microseconds", job.HeldSince, err)
	}
	f.checkTables()

	// Olga takes it from the team: recorded as Olga, the team is cleared.
	job = f.hand(job.ID, f.ops, "", olga, workitem.HowTake, "", "", "")
	h = f.lastHandOver(job.ID)
	if job.AssigneeID != f.ops || job.Team != "" || h.ActorKind != "human" || h.Actor != f.ops ||
		h.Change["how"] != workitem.HowTake || h.Change["from_team"] != "ops" || h.Change["from_assignee"] != nil {
		t.Errorf("take: with %q team %q, event %+v; want Olga alone, by Olga, from the ops team", job.AssigneeID, job.Team, h)
	}
	f.checkTables()

	// Olga submits and approves for Ops, then passes it to Dev with a note. The approval stays.
	job, err = f.actAs(job.ID, "submit", "", olga)
	if err != nil || job.State != "pending_approval" {
		t.Fatalf("submit by Olga = %v, %s", err, job.State)
	}
	job = f.decide(job.ID, f.ops, "ops", engine.Approve, "")
	version := job.Version
	job = f.hand(job.ID, dev, "", olga, workitem.HowPass, "he knows Acme well", "", "")
	h = f.lastHandOver(job.ID)
	if job.AssigneeID != dev || job.HandoffFrom != f.ops || job.HandoffNote != "he knows Acme well" ||
		h.Change["how"] != workitem.HowPass || h.Change["note"] != "he knows Acme well" || h.Actor != f.ops {
		t.Errorf("pass: with %q from %q note %q, event %+v", job.AssigneeID, job.HandoffFrom, job.HandoffNote, h)
	}
	if job.Version != version {
		t.Errorf("a hand-over changed the version from %d to %d", version, job.Version)
	}
	if waiting := f.waitingFor(job.ID); len(waiting) != 1 || waiting[0] != "owner" {
		t.Errorf("after the pass, waiting for %v; want only owner (the Ops approval stays)", waiting)
	}
	f.checkTables()

	// A return (as a send-back will do in part 4): by Seyal, from the person who sent it back.
	job = f.hand(job.ID, f.sales, "", ledger.SystemActor, workitem.HowReturn, "the March statement is missing", dev, "back_to_sales")
	h = f.lastHandOver(job.ID)
	if job.AssigneeID != f.sales || job.HandoffFrom != dev || h.ActorKind != "system" ||
		h.Change["how"] != workitem.HowReturn || h.Change["after"] != "back_to_sales" || h.Change["from_assignee"] != dev {
		t.Errorf("return: with %q from %q, event %+v; want Sales, from Dev, by Seyal, after back_to_sales", job.AssigneeID, job.HandoffFrom, h)
	}
	if job.Version != version {
		t.Errorf("a return changed the version from %d to %d", version, job.Version)
	}

	// Handing it to whoever already has it records nothing.
	before := len(f.events(job.ID, "item.assigned"))
	f.hand(job.ID, f.sales, "", ledger.PersonActor(f.sales), workitem.HowPass, "again", "", "")
	if n := len(f.events(job.ID, "item.assigned")); n != before {
		t.Errorf("a hand-over to the same person was recorded (%d events, want %d)", n, before)
	}
	f.checkTables()
}
