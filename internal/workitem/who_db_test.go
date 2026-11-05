//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// addAdmin adds a made-up person whose only role is admin.
func (f *firm) addAdmin() string {
	f.t.Helper()
	var id string
	f.tx(func(tx pgx.Tx) error {
		if _, err := people.DefineRole(f.ctx, tx, workitem.AdminRole, "Admin", ledger.SystemActor); err != nil {
			return err
		}
		var err error
		if id, err = people.Add(f.ctx, tx, "Chitra Admin", "chitra@acme.example", ledger.SystemActor); err != nil {
			return err
		}
		_, err = people.Grant(f.ctx, tx, id, workitem.AdminRole, ledger.SystemActor)
		return err
	})
	return id
}

func (f *firm) actAs(id, action, reason string, by ledger.Actor) (workitem.Item, error) {
	var it workitem.Item
	err := f.try(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Act(f.ctx, tx, id, action, reason, by)
		return err
	})
	return it, err
}

func (f *firm) mayChange(id string, by ledger.Actor) error {
	var result error
	f.tx(func(tx pgx.Tx) error {
		it, err := workitem.Get(f.ctx, tx, id)
		if err != nil {
			return err
		}
		result = workitem.MayChange(f.ctx, tx, it, by)
		return nil
	})
	return result
}

// lastAs is the "as" of the item's latest move ("admin" when only being an admin allowed it).
func (f *firm) lastAs(id string) string {
	f.t.Helper()
	moves := f.events(id, "item.state_changed")
	if len(moves) == 0 {
		return ""
	}
	if strings.Contains(strings.ReplaceAll(moves[len(moves)-1], " ", ""), `"as":"admin"`) {
		return "admin"
	}
	return ""
}

func (f *firm) checkTables() {
	f.t.Helper()
	f.checkLedger()
	f.tx(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			f.t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}

// TestWhoOnTheSample follows sample job evaluation 0.5 (D68): once the job is with the Ops team, only
// the team (or an admin, recorded as admin) may submit it; Sales, who started it, may not, but may
// still cancel; the Owner role may cancel too. MayChange keeps today's rule for the details.
func TestWhoOnTheSample(t *testing.T) {
	f := newFirm(t)
	chitra := f.addAdmin()
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx pgx.Tx) error {
		f.pv, _, err = playbook.Register(f.ctx, tx, lib.Playbooks["job-evaluation"])
		return err
	})
	asha, olga, omar, mia := ledger.PersonActor(f.sales), ledger.PersonActor(f.ops), ledger.PersonActor(f.owner), ledger.PersonActor(f.maker)
	admin := ledger.PersonActor(chitra)

	toOps := func() workitem.Item {
		t.Helper()
		it := f.start(ready())
		if _, err := f.actAs(it.ID, "send_to_ops", "", olga); err == nil {
			t.Error("Ops sent a draft it doesn't hold (send_to_ops is for whoever has it)")
		}
		it = f.act(it.ID, "send_to_ops", "")
		if it.State != "with_ops" || it.Team != "ops" {
			t.Fatalf("after send_to_ops: %s with team %q", it.State, it.Team)
		}
		return it
	}

	job := toOps()
	moves := len(f.events(job.ID, "item.state_changed"))
	for _, who := range []struct {
		name string
		by   ledger.Actor
	}{{"Sales, who started it", asha}, {"someone with no role", mia}, {"the Owner role", omar}} {
		_, err := f.actAs(job.ID, "submit", "", who.by)
		var na *workitem.NotAllowedError
		if !errors.As(err, &na) || !errors.Is(err, workitem.ErrNotYours) || !strings.Contains(err.Error(), `"submit"`) {
			t.Errorf("submit by %s = %v, want a plain refusal naming the move", who.name, err)
		}
	}
	if n := len(f.events(job.ID, "item.state_changed")); n != moves {
		t.Errorf("a refused move was recorded (%d moves, want %d)", n, moves)
	}

	// MayChange (the details): the starter, the team, an admin and Seyal may; others may not; and an
	// actor that isn't Seyal but has no person never may.
	for _, c := range []struct {
		name string
		by   ledger.Actor
		ok   bool
	}{
		{"the starter", asha, true}, {"the team", olga, true}, {"an admin", admin, true},
		{"Seyal on its own", ledger.SystemActor, true}, {"Seyal for the team", ledger.AgentActor(f.ops), true},
		{"someone else", mia, false}, {"the Owner role", omar, false},
		{"a person with no id", ledger.PersonActor(""), false}, {"an agent for nobody", ledger.AgentActor(""), false},
	} {
		if err := f.mayChange(job.ID, c.by); (err == nil) != c.ok || (err != nil && !errors.Is(err, workitem.ErrNotYours)) {
			t.Errorf("MayChange(%s) = %v, want allowed %v", c.name, err, c.ok)
		}
	}

	if it, err := f.actAs(job.ID, "submit", "", olga); err != nil || it.State != "pending_approval" || f.lastAs(job.ID) != "" {
		t.Errorf("submit by the team = %v, %s, as %q; want pending_approval, not as admin", err, it.State, f.lastAs(job.ID))
	}

	second := toOps()
	if it, err := f.actAs(second.ID, "submit", "", admin); err != nil || it.State != "pending_approval" || f.lastAs(second.ID) != "admin" {
		t.Errorf("submit by an admin = %v, %s, as %q; want pending_approval, as admin", err, it.State, f.lastAs(second.ID))
	}

	third := toOps()
	if it, err := f.actAs(third.ID, "cancel", "client said no", omar); err != nil || it.State != "cancelled" || f.lastAs(third.ID) != "" {
		t.Errorf("cancel by the Owner role = %v, %s, as %q; want cancelled, not as admin", err, it.State, f.lastAs(third.ID))
	}
	fourth := toOps()
	if it, err := f.actAs(fourth.ID, "cancel", "duplicate", asha); err != nil || it.State != "cancelled" {
		t.Errorf("cancel by the starter = %v, %s; want cancelled", err, it.State)
	}
	f.checkTables()
}

// whoCheck is a made-up process: a team checks the work, the lead it names holds it, and only the
// Owner role may sign it off.
const whoCheck = `id: who-check
version: "1"
description: A made-up check a team does and the Owner role signs off.
numbering: "W-{seq:4}"
roles: [sales, ops, owner]
fields:
  - { key: topic, label: Topic, type: text, required: true }
  - { key: lead, label: Lead, type: person }
states: [new, checking, signed, closed]
initial: new
steps:
  new: { owner: starter }
  checking: { owner: "role:ops" }
transitions:
  - { action: check, from: [new], to: checking }
  - { action: sign, from: [checking], to: signed, who: ["role:owner"] }
  - { action: close, from: [new, checking, signed], to: closed, needs: reason }
on:
  checking:
    - { do: assign, to: lead }
`

// TestWhoLimitsAndLostRole: admins don't pass a role: limit (D68, choice 21), and a person holding
// work in a team's step stops counting as its holder once they lose the team's role (choice 22).
func TestWhoLimitsAndLostRole(t *testing.T) {
	f := newFirm(t)
	chitra := f.addAdmin()
	p, err := playbook.Parse([]byte(whoCheck))
	if err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx pgx.Tx) error {
		f.pv, _, err = playbook.Register(f.ctx, tx, p)
		return err
	})
	olga := ledger.PersonActor(f.ops)

	it := f.start(engine.Fields{"topic": "Year-end check", "lead": f.ops})
	if it = f.act(it.ID, "check", ""); it.State != "checking" || it.AssigneeID != f.ops || it.Team != "" {
		t.Fatalf("after check: %s with %q, team %q; want checking with the lead", it.State, it.AssigneeID, it.Team)
	}
	for _, who := range []struct {
		name string
		by   ledger.Actor
	}{{"an admin", ledger.PersonActor(chitra)}, {"the holder", olga}, {"the starter", ledger.PersonActor(f.sales)}} {
		if _, err := f.actAs(it.ID, "sign", "", who.by); !errors.Is(err, workitem.ErrNotYours) {
			t.Errorf("sign by %s = %v, want refused (only the Owner role)", who.name, err)
		}
	}
	if err := f.mayChange(it.ID, olga); err != nil {
		t.Errorf("MayChange(the lead, in Ops) = %v", err)
	}

	f.tx(func(tx pgx.Tx) error {
		_, err := people.Revoke(f.ctx, tx, f.ops, "ops", ledger.SystemActor)
		return err
	})
	if err := f.mayChange(it.ID, olga); !errors.Is(err, workitem.ErrNotYours) {
		t.Errorf("MayChange(the lead, no longer in Ops) = %v, want refused", err)
	}
	if _, err := f.actAs(it.ID, "close", "not needed", olga); !errors.Is(err, workitem.ErrNotYours) {
		t.Errorf("close by the lead who left Ops = %v, want refused", err)
	}
	if it, err := f.actAs(it.ID, "sign", "", ledger.PersonActor(f.owner)); err != nil || it.State != "signed" {
		t.Errorf("sign by the Owner role = %v, %s; want signed", err, it.State)
	}
	f.checkTables()
}
