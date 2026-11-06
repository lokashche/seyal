//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

// handFirm is the test firm on the shipped samples (job evaluation 0.5 and other work), with a second
// Ops person (Dev) and an admin (Chitra), and logins for everyone but Mia the maker: work only goes to
// active people with a login (D68, choice 12).
type handFirm struct {
	*firm
	dev, admin string
	other      string // the Other work version
}

func newHandFirm(t *testing.T) *handFirm {
	t.Helper()
	f := &handFirm{firm: newFirm(t)}
	f.admin = f.addAdmin()
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx pgx.Tx) error {
		if f.pv, _, err = playbook.Register(f.ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		if f.other, _, err = playbook.Register(f.ctx, tx, lib.Playbooks["other-work"]); err != nil {
			return err
		}
		if f.dev, err = people.Add(f.ctx, tx, "Dev Ops", "dev@acme.example", ledger.SystemActor); err != nil {
			return err
		}
		_, err = people.Grant(f.ctx, tx, f.dev, "ops", ledger.SystemActor)
		return err
	})
	owner, _ := testdb.Connect(t)
	testdb.GiveLogins(t, owner, f.tenant, f.dev, f.admin) // newFirm gave Sales, Ops and the owner theirs
	return f
}

func (f *handFirm) pass(id string, to workitem.PassOn, by string) (workitem.Item, string, error) {
	var it workitem.Item
	var notice string
	err := f.try(func(tx pgx.Tx) error {
		var err error
		it, notice, err = workitem.Pass(f.ctx, tx, id, to, ledger.PersonActor(by))
		return err
	})
	return it, notice, err
}

func (f *handFirm) take(id, seen, by string) (workitem.Item, error) {
	var it workitem.Item
	err := f.try(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Take(f.ctx, tx, id, seen, "panel", ledger.PersonActor(by))
		return err
	})
	return it, err
}

func (f *handFirm) setAs(id string, values engine.Fields, by string) (workitem.Item, error) {
	var it workitem.Item
	err := f.try(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.SetFields(f.ctx, tx, id, values, ledger.PersonActor(by))
		return err
	})
	return it, err
}

func (f *handFirm) get(id string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Get(f.ctx, tx, id)
		return err
	})
	return it
}

func wantRefused(t *testing.T, what string, err error, words string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), words) {
		t.Errorf("%s: error %v; want one saying %q", what, err, words)
	}
}

// TestTakeAndPass follows one job in "with_ops" (D68, M1 and M2): two Ops people take it at the same
// moment and only one gets it; work goes only to people the step allows; a form from before a hand-over
// changes nothing; a holder who lost the Ops role may only pass it on; an admin may pass outside the
// list with a warning; a disabled person or one with no login never gets work; and `seyal verify`
// stays clean.
func TestTakeAndPass(t *testing.T) {
	f := newHandFirm(t)
	job := f.start(ready())
	job = f.act(job.ID, "send_to_ops", "")
	if job.Team != "ops" || job.HolderKey() != "team:ops" {
		t.Fatalf("sent to Ops: with %q / team %q", job.AssigneeID, job.Team)
	}
	version := job.Version

	// Olga and Dev both press Take it on the same card at the same moment: one gets it.
	var wg sync.WaitGroup
	errs := map[string]error{}
	var mu sync.Mutex
	start := make(chan struct{})
	for _, p := range []string{f.ops, f.dev} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.take(job.ID, "team:ops", p)
			mu.Lock()
			errs[p] = err
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	winner, loser := f.ops, f.dev
	if errs[f.ops] != nil {
		winner, loser = f.dev, f.ops
	}
	if errs[winner] != nil || errs[loser] == nil {
		t.Fatalf("two takers: %v; want exactly one to get it", errs)
	}
	wantRefused(t, "the second taker", errs[loser], "took it a moment ago, so nothing was changed. Ask")
	job = f.get(job.ID)
	h := f.lastHandOver(job.ID)
	if job.AssigneeID != winner || job.Team != "" || h.Actor != winner || h.ActorKind != "human" ||
		h.Change["how"] != workitem.HowTake || h.Change["from_team"] != "ops" || h.Change["via"] != "panel" {
		t.Errorf("after the take: with %q team %q, event %+v; want the winner alone, recorded as them", job.AssigneeID, job.Team, h)
	}
	if n := len(f.events(job.ID, "item.assigned")); n != 3 { // the starter, the Ops team, the take
		t.Errorf("%d hand-overs recorded; want 3 (the losing take records nothing)", n)
	}
	_, err := f.take(job.ID, "", loser)
	wantRefused(t, "taking an item someone has", err, "has it. Ask")
	_, err = f.take(job.ID, "", f.sales)
	wantRefused(t, "taking as Sales", err, "has it. Ask") // not with the team any more: it is the winner's
	f.checkTables()

	// Only people in Ops (or the Ops team) may receive it here; the starter may not pass it.
	_, _, err = f.pass(job.ID, workitem.PassOn{Person: f.sales}, winner)
	wantRefused(t, "passing to Sales", err, "can't receive it in this step. It can go to:")
	_, _, err = f.pass(job.ID, workitem.PassOn{Person: f.ops}, f.sales)
	if !errors.Is(err, workitem.ErrNotYours) || strings.Count(err.Error(), "only whoever") != 1 {
		t.Errorf("the starter passing work someone else has: %v; want ErrNotYours, said once", err)
	}
	_, _, err = f.pass(job.ID, workitem.PassOn{Person: winner}, winner)
	wantRefused(t, "passing to whoever has it", err, "it's already with")

	// The winner passes it to the loser, with a note.
	job, notice, err := f.pass(job.ID, workitem.PassOn{Person: loser, Note: " knows them well ", Via: "panel",
		Seen: "person:" + winner}, winner)
	h = f.lastHandOver(job.ID)
	if err != nil || notice != "" || job.AssigneeID != loser || job.HandoffFrom != winner || job.HandoffNote != "knows them well" ||
		h.Actor != winner || h.Change["how"] != workitem.HowPass || h.Change["note"] != "knows them well" || h.Change["via"] != "panel" {
		t.Errorf("pass: %v %q, with %q from %q note %q, event %+v", err, notice, job.AssigneeID, job.HandoffFrom, job.HandoffNote, h)
	}
	if job.Version != version {
		t.Errorf("a pass changed the version from %d to %d", version, job.Version)
	}
	// The winner's page still showed them holding it: that form now does nothing.
	_, _, err = f.pass(job.ID, workitem.PassOn{Team: "ops", Seen: "person:" + winner}, winner)
	wantRefused(t, "a pass from an old page", err, "a moment ago, so nothing was changed")

	// The loser loses the Ops role: in this team step they may only pass it on (or give it back).
	f.tx(func(tx pgx.Tx) error {
		_, err := people.Revoke(f.ctx, tx, loser, "ops", ledger.SystemActor)
		return err
	})
	if _, err := f.setAs(job.ID, engine.Fields{"timeline_days": 3.0}, loser); !errors.Is(err, workitem.ErrNotYours) {
		t.Errorf("a holder without the role changing a detail: %v; want ErrNotYours", err)
	}
	if _, err := f.actAs(job.ID, "submit", "", ledger.PersonActor(loser)); !errors.Is(err, workitem.ErrNotYours) {
		t.Errorf("a holder without the role submitting: %v; want ErrNotYours", err)
	}
	job, _, err = f.pass(job.ID, workitem.PassOn{Team: "ops"}, loser) // Give back to the team
	if err != nil || job.Team != "ops" || job.AssigneeID != "" {
		t.Errorf("giving it back to the team: %v, with %q team %q", err, job.AssigneeID, job.Team)
	}

	// Chitra (admin, not in Ops) passes it to Sales: allowed, with a warning, recorded as an admin.
	job, notice, err = f.pass(job.ID, workitem.PassOn{Person: f.sales, Note: "client call first"}, f.admin)
	h = f.lastHandOver(job.ID)
	if err != nil || job.AssigneeID != f.sales || !strings.Contains(notice, "as an admin") ||
		!strings.Contains(notice, "people in ops") || h.Change["as"] != "admin" || h.Actor != f.admin ||
		job.HandoffFrom != f.admin {
		t.Errorf("admin pass: %v %q, with %q from %q, event %+v", err, notice, job.AssigneeID, job.HandoffFrom, h)
	}

	// Nobody gets work who is disabled or has no login, not even from an admin.
	f.tx(func(tx pgx.Tx) error {
		_, err := people.SetActive(f.ctx, tx, winner, false, ledger.SystemActor)
		return err
	})
	_, _, err = f.pass(job.ID, workitem.PassOn{Person: winner}, f.admin)
	wantRefused(t, "passing to a disabled person", err, "is no longer active")
	_, _, err = f.pass(job.ID, workitem.PassOn{Person: f.maker}, f.admin)
	wantRefused(t, "passing to someone with no login", err, "hasn't joined yet")

	// Nobody is left in Ops: an admin may still send it to the team, with a warning, and it is stuck.
	job, notice, err = f.pass(job.ID, workitem.PassOn{Team: "ops"}, f.admin)
	if err != nil || job.Team != "ops" || !strings.Contains(notice, "Nobody is in the ops team yet") {
		t.Errorf("passing to an empty team: %v, team %q, notice %q", err, job.Team, notice)
	}
	f.tx(func(tx pgx.Tx) error {
		p, err := workitem.PlaybookFor(f.ctx, tx, job)
		if err != nil {
			return err
		}
		members, err := workitem.Members(f.ctx, tx)
		if !workitem.NobodyCanTake(p, job, members) {
			t.Error("with nobody in Ops, the job should be one nobody can take")
		}
		return err
	})
	if job.Version != version {
		t.Errorf("hand-overs changed the version from %d to %d", version, job.Version)
	}
	f.checkTables()
}

// TestOwnerFieldIsAPass: in a step owned by a person field (Other work: "Who does it"), changing that
// field hands the item over, so it is a pass on with all its checks (D68, choice 14). The starter
// can't give away work someone else now has; whoever has it can; an admin can; nobody without a login
// gets it. Pass on sets the field (the version goes up), and a team can't have it there.
func TestOwnerFieldIsAPass(t *testing.T) {
	f := newHandFirm(t)
	var item workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		item, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: f.other, Via: "chat",
			Fields: engine.Fields{"what": "Water the plants"}, By: ledger.PersonActor(f.sales)})
		return err
	})
	if item.AssigneeID != f.sales {
		t.Fatalf("a new item is with %q; want the starter", item.AssigneeID)
	}

	item, err := f.setAs(item.ID, engine.Fields{"who": f.ops}, f.sales)
	h := f.lastHandOver(item.ID)
	if err != nil || item.AssigneeID != f.ops || h.Actor != f.sales || h.ActorKind != "human" || h.Change["how"] != workitem.HowPass {
		t.Fatalf("the starter naming Olga while holding it: %v, with %q, event %+v; want a pass by the starter", err, item.AssigneeID, h)
	}

	// Now Olga has it: the starter can't give it to Dev by editing the field.
	before := f.get(item.ID)
	_, err = f.setAs(item.ID, engine.Fields{"who": f.dev}, f.sales)
	if !errors.Is(err, workitem.ErrNotYours) || !strings.Contains(err.Error(), `change "Who does it", because it says who has the work`) ||
		strings.Count(err.Error(), "only whoever") != 1 {
		t.Errorf("the starter editing who does it on Olga's item: %v; want refused", err)
	}
	if after := f.get(item.ID); after.AssigneeID != f.ops || after.Version != before.Version {
		t.Errorf("a refused edit changed the item: with %q, version %d → %d", after.AssigneeID, before.Version, after.Version)
	}
	// The starter may still change other details (D55): only the owner field carries the hand-over.
	if _, err := f.setAs(item.ID, engine.Fields{"due_on": "2030-01-31"}, f.sales); err != nil {
		t.Errorf("the starter changing the due date: %v", err)
	}

	// Olga may; nobody with no login may receive it.
	if _, err := f.setAs(item.ID, engine.Fields{"who": f.maker}, f.ops); err == nil || !strings.Contains(err.Error(), "hasn't joined yet") {
		t.Errorf("naming someone with no login: %v; want refused", err)
	}
	item, err = f.setAs(item.ID, engine.Fields{"who": f.dev}, f.ops)
	if h = f.lastHandOver(item.ID); err != nil || item.AssigneeID != f.dev || h.Actor != f.ops || h.Change["how"] != workitem.HowPass {
		t.Errorf("Olga naming Dev: %v, with %q, event %+v", err, item.AssigneeID, h)
	}

	// Pass on in this step sets the field too, so the two always agree; the version goes up.
	version := item.Version
	item, _, err = f.pass(item.ID, workitem.PassOn{Person: f.ops, Note: "back to you"}, f.dev)
	if err != nil || item.AssigneeID != f.ops || item.Fields["who"] != f.ops || item.Version != version+1 ||
		item.HandoffNote != "back to you" {
		t.Errorf("Pass on in a person field's step: %v, with %q, who %v, version %d → %d, note %q",
			err, item.AssigneeID, item.Fields["who"], version, item.Version, item.HandoffNote)
	}
	if _, _, err := f.pass(item.ID, workitem.PassOn{Team: "ops"}, f.ops); err == nil {
		t.Error("a team was given an item whose step names one person")
	}

	// An admin may change it, recorded as an admin.
	item, err = f.setAs(item.ID, engine.Fields{"who": f.sales}, f.admin)
	if h = f.lastHandOver(item.ID); err != nil || item.AssigneeID != f.sales || h.Change["as"] != "admin" {
		t.Errorf("an admin naming Sales: %v, with %q, event %+v", err, item.AssigneeID, h)
	}
	f.checkTables()
}

// ownerless is a made-up process whose steps name no owner, like every process version from before
// steps had owners (D68): an item stays with nobody, and its starter is the one who acts.
const ownerless = `id: ownerless
version: "1"
description: A made-up process with no step owners.
numbering: "N-{seq:4}"
fields:
  - { key: topic, label: Topic, type: text }
states: [open, done]
initial: open
transitions:
  - { action: finish, from: [open], to: done }
`

// TestTakeNeedsNoStarter: an item nobody has but its starter is the starter's, so a colleague can't
// take it (D55: they may read it and write notes); they are told to ask the starter. Once the starter
// is no longer active, anyone the step allows may take it (D68, choice 13).
func TestTakeNeedsNoStarter(t *testing.T) {
	f := newHandFirm(t)
	p, err := playbook.Parse([]byte(ownerless))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	var item workitem.Item
	f.tx(func(tx pgx.Tx) error {
		if pv, _, err = playbook.Register(f.ctx, tx, p); err != nil {
			return err
		}
		item, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat",
			Fields: engine.Fields{"topic": "Tidy the shared folder"}, By: ledger.PersonActor(f.sales)})
		return err
	})
	if item.HolderKey() != "nobody" || item.CreatedBy != f.sales {
		t.Fatalf("a new ownerless item: holder %q, starter %q; want nobody, and Sales as the starter", item.HolderKey(), item.CreatedBy)
	}

	_, err = f.take(item.ID, "nobody", f.ops)
	if !errors.Is(err, workitem.ErrNotYours) || !strings.Contains(err.Error(), "started it and has it. Ask") {
		t.Errorf("Olga taking the starter's item: %v; want refused, asking the starter", err)
	}
	_, err = f.take(item.ID, "nobody", f.sales)
	wantRefused(t, "the starter taking their own item", err, "it's already yours")
	if after := f.get(item.ID); after.AssigneeID != "" || len(f.events(item.ID, "item.assigned")) != 0 {
		t.Errorf("a refused take changed the item: with %q", after.AssigneeID)
	}

	// The starter leaves: now the item is free, and Olga may take it.
	f.tx(func(tx pgx.Tx) error {
		_, err := people.SetActive(f.ctx, tx, f.sales, false, ledger.SystemActor)
		return err
	})
	if item, err = f.take(item.ID, "nobody", f.ops); err != nil || item.AssigneeID != f.ops {
		t.Errorf("Olga taking it after the starter left: %v, with %q", err, item.AssigneeID)
	}
	f.checkTables()
}
