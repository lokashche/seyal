//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// landing is the hint under a move's button for one person (workitem.Landing).
func (f *handFirm) landing(id, action, person string) string {
	f.t.Helper()
	var hint string
	f.tx(func(tx pgx.Tx) error {
		it, err := workitem.Get(f.ctx, tx, id)
		if err != nil {
			return err
		}
		rows, _ := tx.Query(f.ctx, `SELECT role_key FROM person_roles WHERE person_id::text = $1 AND revoked_at IS NULL`, person)
		roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		hint, err = workitem.Landing(f.ctx, tx, it, action, workitem.Actor(person, roles))
		return err
	})
	return hint
}

func (f *handFirm) setActive(person string, active bool) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		_, err := people.SetActive(f.ctx, tx, person, active, ledger.SystemActor)
		return err
	})
}

func (f *handFirm) posts(id string) string {
	return strings.Join(f.events(id, "item.posted"), "\n")
}

// TestSendBackToTheLastPerson follows sample job evaluation 0.5 (D68, M4): Dev takes the job from the
// Ops team and sends it back with a reason. It goes back to whoever had the draft (Sales), from Dev, with
// the reason; the hint said so before. Sales sends it again: because with_ops says "returns_to: last",
// it goes straight back to Dev, not to the whole team. The version never changes and `seyal verify`
// stays clean.
func TestSendBackToTheLastPerson(t *testing.T) {
	f := newHandFirm(t)
	job := f.start(ready())
	job = f.act(job.ID, "send_to_ops", "")
	job, err := f.take(job.ID, "team:ops", f.dev)
	if err != nil {
		t.Fatal(err)
	}
	version := job.Version

	if hint := f.landing(job.ID, "back_to_sales", f.dev); hint != "Goes back to Test Person." {
		t.Errorf("hint before sending back = %q; want it to name Sales", hint)
	}
	job, err = f.actAs(job.ID, "back_to_sales", " the March bank statement is missing ", ledger.PersonActor(f.dev))
	h := f.lastHandOver(job.ID)
	if err != nil || job.State != "draft" || job.AssigneeID != f.sales || job.HandoffFrom != f.dev ||
		job.HandoffNote != "the March bank statement is missing" {
		t.Fatalf("sent back: %v, %s, with %q from %q note %q; want draft, with Sales, from Dev, with the reason",
			err, job.State, job.AssigneeID, job.HandoffFrom, job.HandoffNote)
	}
	if h.ActorKind != "system" || h.Change["how"] != workitem.HowReturn || h.Change["from_assignee"] != f.dev ||
		h.Change["after"] != "back_to_sales" || h.Change["note"] != "the March bank statement is missing" {
		t.Errorf("the return event = %+v; want how return, by Seyal, from Dev, after back_to_sales, with the reason", h)
	}
	if job.Version != version {
		t.Errorf("sending back changed the version from %d to %d", version, job.Version)
	}
	f.checkTables()

	if hint := f.landing(job.ID, "send_to_ops", f.sales); hint != "Goes back to Dev Ops." {
		t.Errorf("hint before sending again = %q; want it to name Dev", hint)
	}
	job, err = f.actAs(job.ID, "send_to_ops", "", ledger.PersonActor(f.sales))
	h = f.lastHandOver(job.ID)
	if err != nil || job.State != "with_ops" || job.AssigneeID != f.dev || job.Team != "" ||
		h.Change["how"] != workitem.HowReturn || h.Change["from_assignee"] != f.sales || h.Change["after"] != "send_to_ops" {
		t.Errorf("sent again: %v, %s, with %q team %q, event %+v; want with_ops, straight back to Dev, from Sales",
			err, job.State, job.AssigneeID, job.Team, h)
	}
	f.checkTables()
}

// TestSendBackToATeam: if the team had the step when it was sent back (Olga took it and gave it back
// to the team), the team gets it back as a team, not Olga. If the last person is no longer active or
// lost the step's role, the step's owner gets it.
func TestSendBackToATeam(t *testing.T) {
	f := newHandFirm(t)
	job := f.start(ready())
	job = f.act(job.ID, "send_to_ops", "")
	if _, err := f.take(job.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.pass(job.ID, workitem.PassOn{Team: "ops"}, f.ops); err != nil { // Give back to the team
		t.Fatal(err)
	}
	job, err := f.actAs(job.ID, "back_to_sales", "the client name is wrong", ledger.PersonActor(f.dev))
	if err != nil || job.AssigneeID != f.sales {
		t.Fatalf("sent back by Dev from the team: %v, with %q", err, job.AssigneeID)
	}
	if hint := f.landing(job.ID, "send_to_ops", f.sales); hint != "Goes back to the ops team." {
		t.Errorf("hint = %q; want the team", hint)
	}
	job = f.act(job.ID, "send_to_ops", "")
	if job.Team != "ops" || job.AssigneeID != "" || f.lastHandOver(job.ID).Change["how"] != workitem.HowReturn {
		t.Errorf("sent again: with %q team %q; want the ops team, as a return", job.AssigneeID, job.Team)
	}

	// Dev takes it and sends it back, then is disabled: sent again, it goes to the step's owner (the team).
	if _, err := f.take(job.ID, "team:ops", f.dev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.actAs(job.ID, "back_to_sales", "missing PAN", ledger.PersonActor(f.dev)); err != nil {
		t.Fatal(err)
	}
	f.setActive(f.dev, false)
	if hint := f.landing(job.ID, "send_to_ops", f.sales); hint != "" {
		t.Errorf("hint with Dev gone = %q; want none (it goes to the team, as usual)", hint)
	}
	job = f.act(job.ID, "send_to_ops", "")
	if job.Team != "ops" || job.AssigneeID != "" || f.lastHandOver(job.ID).Change["how"] != workitem.HowStep {
		t.Errorf("sent again with Dev disabled: with %q team %q; want the ops team (the step's owner)", job.AssigneeID, job.Team)
	}

	// Olga takes it and sends it back, then loses the Ops role: the same.
	if _, err := f.take(job.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	if _, err := f.actAs(job.ID, "back_to_sales", "missing GSTIN", ledger.PersonActor(f.ops)); err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx pgx.Tx) error {
		_, err := people.Revoke(f.ctx, tx, f.ops, "ops", ledger.SystemActor)
		return err
	})
	if hint := f.landing(job.ID, "send_to_ops", f.sales); !strings.Contains(hint, "Nobody is in the ops team yet") {
		t.Errorf("hint with nobody left in Ops = %q; want the empty team warning", hint)
	}
	job = f.act(job.ID, "send_to_ops", "")
	if job.Team != "ops" || job.AssigneeID != "" {
		t.Errorf("sent again with Olga out of Ops: with %q team %q; want the ops team", job.AssigneeID, job.Team)
	}
	f.checkTables()
}

// sendBackDesk is a made-up process (no real business) whose first step posts a welcome on entering
// it, so a test can see that sending work back doesn't run the step's actions again.
const sendBackDesk = `id: desk-return
version: "1"
description: A made-up request a team handles and can send back.
numbering: "D-{seq:4}"
roles: [ops]
fields:
  - { key: topic, label: Topic, type: text, required: true }
states: [new, with_team, closed]
initial: new
steps:
  new: { owner: starter, returns_to: last }
  with_team: { owner: "role:ops", returns_to: last }
transitions:
  - { action: send, from: [new], to: with_team }
  - { action: ask_again, from: [with_team], to: new, needs: reason, back: true, who: [holder] }
  - { action: close, from: [new, with_team], to: closed, needs: reason }
on:
  new:
    - { do: post, text: "Welcome. Tell us the topic." }
`

// TestSendBackSkipsActionsAndStaysWithTheSender: sending work back doesn't run the step's on: actions
// again (D68, choice 8). When whoever had the step and the step's owner are both gone (the starter
// left), the work stays with the person who sent it back, the hint warned them, and the thread says so
// (choice 7).
func TestSendBackSkipsActionsAndStaysWithTheSender(t *testing.T) {
	f := newHandFirm(t)
	p, err := playbook.Parse([]byte(sendBackDesk))
	if err != nil {
		t.Fatal(err)
	}
	var item workitem.Item
	f.tx(func(tx pgx.Tx) error {
		pv, _, err := playbook.Register(f.ctx, tx, p)
		if err != nil {
			return err
		}
		item, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat",
			Fields: engine.Fields{"topic": "New desk chair"}, By: ledger.PersonActor(f.sales)})
		return err
	})
	item = f.act(item.ID, "send", "")
	if _, err := f.take(item.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	item, err = f.actAs(item.ID, "ask_again", "which colour?", ledger.PersonActor(f.ops))
	if err != nil || item.AssigneeID != f.sales {
		t.Fatalf("sent back: %v, with %q", err, item.AssigneeID)
	}
	if n := strings.Count(f.posts(item.ID), "Welcome."); n != 1 {
		t.Errorf("the welcome was posted %d times; want once (sending back doesn't run the step again)", n)
	}

	// Back to the team (Olga gets it again), then the starter leaves and Olga sends it back once more.
	item = f.act(item.ID, "send", "")
	if item.AssigneeID != f.ops {
		t.Fatalf("sent again: with %q; want Olga", item.AssigneeID)
	}
	f.setActive(f.sales, false)
	want := "Test Person is no longer active and started this item, so it will stay with you. An admin can pass it on."
	if hint := f.landing(item.ID, "ask_again", f.ops); hint != want {
		t.Errorf("hint = %q; want %q", hint, want)
	}
	// An admin who doesn't have it would get it by sending it back, so the hint says it goes to them.
	want = "Test Person is no longer active and started this item, so it will go to you. An admin can pass it on."
	if hint := f.landing(item.ID, "ask_again", f.admin); hint != want {
		t.Errorf("hint for an admin = %q; want %q", hint, want)
	}
	item, err = f.actAs(item.ID, "ask_again", "which size?", ledger.PersonActor(f.ops))
	if err != nil || item.State != "new" || item.AssigneeID != f.ops {
		t.Errorf("sent back with the starter gone: %v, %s, with %q; want new, still with Olga", err, item.State, item.AssigneeID)
	}
	if !strings.Contains(f.posts(item.ID), "Test Person is no longer active and started this item, so it stays with Olga Ops. An admin can pass it on.") {
		t.Errorf("the thread doesn't say where it stays: %s", f.posts(item.ID))
	}
	f.checkTables()
}

// refuse records a refusal of slot by person.
func (f *handFirm) refuse(id, person, slot, reason string) workitem.Item {
	f.t.Helper()
	return f.decide(id, person, slot, engine.Reject, reason)
}

// TestRefusalComesBack follows sample job evaluation 0.5, which says on_reject_back: a refused approval
// sends the job back to whoever had the draft, "From Omar" (who refused) with his reason, not from the
// holder. Sent again, it goes back to whoever had with_ops, and on resubmitting everyone is asked again
// (the version went up). If the starter has left, the refusal doesn't land on them: it stays with
// whoever refused it.
func TestRefusalComesBack(t *testing.T) {
	f := newHandFirm(t)
	job := f.start(ready())
	job = f.act(job.ID, "send_to_ops", "")
	if _, err := f.take(job.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	job, err := f.actAs(job.ID, "submit", "", ledger.PersonActor(f.ops))
	if err != nil || job.State != "pending_approval" {
		t.Fatalf("submit: %v, %s", err, job.State)
	}
	first := job.Version
	job = f.decide(job.ID, f.ops, "ops", engine.Approve, "")
	job = f.refuse(job.ID, f.owner, "owner", "fee too low for this client")
	h := f.lastHandOver(job.ID)
	if job.State != "draft" || job.AssigneeID != f.sales || job.HandoffFrom != f.owner || job.HandoffNote != "fee too low for this client" ||
		h.Change["how"] != workitem.HowReturn || h.Change["after"] != "rejected" || h.Change["from_assignee"] != f.owner {
		t.Fatalf("after the refusal: %s, with %q from %q note %q, event %+v; want draft, with Sales, from Omar, his reason",
			job.State, job.AssigneeID, job.HandoffFrom, job.HandoffNote, h)
	}

	job = f.act(job.ID, "send_to_ops", "")
	if job.AssigneeID != f.ops {
		t.Errorf("sent again: with %q; want Olga, who had with_ops", job.AssigneeID)
	}
	job, err = f.actAs(job.ID, "submit", "", ledger.PersonActor(f.ops))
	if err != nil || job.Version <= first {
		t.Fatalf("submitted again: %v, version %d (first round %d); want a new version", err, job.Version, first)
	}
	if waiting := f.waitingFor(job.ID); len(waiting) != 2 || waiting[0] != "ops" || waiting[1] != "owner" {
		t.Errorf("waiting for %v; want ops and owner again (the first round doesn't count)", waiting)
	}
	f.checkTables()

	// The starter leaves; a second refusal stays with Omar, who refused it.
	f.setActive(f.sales, false)
	job = f.refuse(job.ID, f.owner, "owner", "still too low")
	if job.State != "draft" || job.AssigneeID != f.owner {
		t.Errorf("refused with the starter gone: %s, with %q; want draft, with Omar (not the disabled starter)", job.State, job.AssigneeID)
	}
	// Olga had it, so it doesn't "stay" with Omar: it goes to him.
	if !strings.Contains(f.posts(job.ID), "Test Person is no longer active and started this item, so it goes to Omar Owner.") {
		t.Errorf("the thread doesn't say where it stays: %s", f.posts(job.ID))
	}
	f.checkTables()
}

// TestFeeAfterSendBack: sample 0.5 copies the fee into "Fee proposed by Sales" each time Sales sends the
// job (D69 review fix 2). So if Ops changes the fee and submits, Sales is asked. If Ops changes it and
// sends the job back, and Sales sends it again, Sales has accepted that fee by sending it; but if Ops
// then changes it again, Sales is asked again.
func TestFeeAfterSendBack(t *testing.T) {
	f := newHandFirm(t)
	olga := ledger.PersonActor(f.ops)
	job := f.start(ready())
	job = f.act(job.ID, "send_to_ops", "")
	if _, err := f.take(job.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	set := func(fee float64) {
		t.Helper()
		if _, err := f.setAs(job.ID, engine.Fields{"fee_agreed": fee}, f.ops); err != nil {
			t.Fatal(err)
		}
	}
	set(1500000)
	if _, err := f.actAs(job.ID, "back_to_sales", "the fee had to go up", olga); err != nil {
		t.Fatal(err)
	}
	job = f.act(job.ID, "send_to_ops", "") // Sales sends it again at the new fee
	if job.Fields["fee_proposed"] != 1500000.0 || job.AssigneeID != f.ops {
		t.Fatalf("sent again: fee proposed %v, with %q; want the fee Sales sent, and Olga", job.Fields["fee_proposed"], job.AssigneeID)
	}
	set(1800000) // Ops changes it again
	job, err := f.actAs(job.ID, "submit", "", olga)
	if err != nil {
		t.Fatal(err)
	}
	if waiting := f.waitingFor(job.ID); !strings.Contains(strings.Join(waiting, " "), "sales") {
		t.Errorf("waiting for %v; want Sales asked about a fee Sales didn't send", waiting)
	}
	f.checkTables()
}

// TestPowerFields: the Maker gets the job once it is approved (an "assign" action), so it carries power
// (D68, choice 14). While Ops has the job, Sales (who started it) may no longer change the Maker, not
// even by picking a work type that fills it in (D72 review fix); Olga, who has it, may; an admin may.
func TestPowerFields(t *testing.T) {
	f := newHandFirm(t)
	job := f.start(ready())
	if _, err := f.setAs(job.ID, engine.Fields{"maker": f.dev}, f.sales); err != nil {
		t.Errorf("Sales changing the Maker while Sales has the job: %v", err)
	}
	job = f.act(job.ID, "send_to_ops", "")
	_, err := f.setAs(job.ID, engine.Fields{"maker": f.sales}, f.sales)
	if !errors.Is(err, workitem.ErrNotYours) || !strings.Contains(err.Error(), `change "Maker", because it decides who gets the work`) {
		t.Errorf("Sales changing the Maker while Ops has the job: %v; want refused", err)
	}
	if _, err := f.setAs(job.ID, engine.Fields{"reason": "Books for the new year"}, f.sales); err != nil {
		t.Errorf("Sales changing another detail (D55): %v", err)
	}
	if _, err := f.setAs(job.ID, engine.Fields{"client": "acme"}, f.sales); err != nil {
		t.Errorf("Sales picking the client again (it fills in no maker): %v", err)
	}
	// Picking the work type again would put back its maker (Mia) instead of Dev, whom Sales chose.
	_, err = f.setAs(job.ID, engine.Fields{"nature_of_work": "monthly-books"}, f.sales)
	if !errors.Is(err, workitem.ErrNotYours) || !strings.Contains(err.Error(), `change "Maker"`) {
		t.Errorf("Sales picking the work type (it fills in the maker) while Ops has the job: %v; want refused", err)
	}
	if _, err := f.take(job.ID, "team:ops", f.ops); err != nil {
		t.Fatal(err)
	}
	if job, err = f.setAs(job.ID, engine.Fields{"nature_of_work": "monthly-books"}, f.ops); err != nil || job.Fields["maker"] != f.maker {
		t.Errorf("Olga picking the work type: %v, maker %v; want the work type's maker", err, job.Fields["maker"])
	}
	if job, err = f.setAs(job.ID, engine.Fields{"maker": f.dev}, f.ops); err != nil || job.Fields["maker"] != f.dev {
		t.Errorf("Olga changing the Maker: %v, maker %v", err, job.Fields["maker"])
	}
	_, err = f.setAs(job.ID, engine.Fields{"nature_of_work": "monthly-books"}, f.sales)
	if !errors.Is(err, workitem.ErrNotYours) || f.get(job.ID).Fields["maker"] != f.dev {
		t.Errorf("Sales picking the work type after Olga chose Dev: %v, maker %v; want refused, Dev kept", err, f.get(job.ID).Fields["maker"])
	}
	if job, err = f.setAs(job.ID, engine.Fields{"maker": f.ops}, f.admin); err != nil || job.Fields["maker"] != f.ops {
		t.Errorf("an admin changing the Maker: %v, maker %v", err, job.Fields["maker"])
	}
	f.checkTables()
}

// deskDoer is a made-up task whose step belongs to whoever "Who does it" names, and picking a kind of
// work fills that field in (from the work type's maker).
const deskDoer = `id: desk-doer
version: "1"
description: A made-up task one named person does.
numbering: "K-{seq:4}"
fields:
  - { key: kind_of_work, label: Kind of work, type: master, kind: work_type, prefill: { doer: maker } }
  - { key: doer, label: Who does it, type: person }
states: [open, done]
initial: open
steps:
  open: { owner: "field:doer" }
transitions:
  - { action: finish, from: [open], to: done }
`

// TestOwnerFieldFilledByAListEntry: when picking a kind of work fills in the step's owner field, it is
// a pass on like typing the name (D72 review fix): the item goes to that person, so the field and the
// holder agree. Someone who no longer has it can't change it that way.
func TestOwnerFieldFilledByAListEntry(t *testing.T) {
	f := newHandFirm(t)
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	p, err := playbook.Parse([]byte(deskDoer))
	if err != nil {
		t.Fatal(err)
	}
	var item workitem.Item
	f.tx(func(tx pgx.Tx) error {
		if _, _, err := master.Save(f.ctx, tx, lib.Masters["work_type"], "dev-books", "Books Dev does",
			engine.Fields{"maker": f.dev}, ledger.SystemActor); err != nil {
			return err
		}
		pv, _, err := playbook.Register(f.ctx, tx, p)
		if err != nil {
			return err
		}
		item, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: ledger.PersonActor(f.sales)})
		return err
	})
	if item.AssigneeID != f.sales {
		t.Fatalf("started: with %q; want the starter", item.AssigneeID)
	}
	item, err = f.setAs(item.ID, engine.Fields{"kind_of_work": "dev-books"}, f.sales)
	if err != nil || item.Fields["doer"] != f.dev || item.AssigneeID != f.dev || f.lastHandOver(item.ID).Change["how"] != workitem.HowPass {
		t.Fatalf("picking a kind of work Dev does: %v, doer %v, with %q; want it passed to Dev", err, item.Fields["doer"], item.AssigneeID)
	}
	_, err = f.setAs(item.ID, engine.Fields{"kind_of_work": "monthly-books"}, f.sales)
	if !errors.Is(err, workitem.ErrNotYours) || f.get(item.ID).AssigneeID != f.dev {
		t.Errorf("the starter, who no longer has it, picking another kind of work: %v; want refused, still with Dev", err)
	}
	f.checkTables()
}
