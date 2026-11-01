//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

const handOff = `id: hand-off
version: "1"
description: Ask, hand over to a team, or to a lead when it is new.
numbering: "H-{seq:4}"
roles: [sales, ops]
fields:
  - { key: who, label: Who, type: text, required: true }
  - { key: kind, label: Kind, type: choice, options: [usual, new], required: true }
  - { key: amount, label: Amount, type: money }
  - { key: proposed, label: Proposed amount, type: money, readonly: true }
  - { key: lead, label: Lead, type: person }
states: [asking, with_team, studying, done]
initial: asking
steps:
  asking: { owner: starter, asks: [who, kind, amount], needs: [who, kind] }
  with_team: { owner: "role:ops", asks: [amount], needs: [amount] }
  studying: { owner: "field:lead" }
transitions:
  - { action: hand_over, from: [asking], to: studying, when: required_filled, if: { field: kind, eq: new } }
  - { action: hand_over, from: [asking], to: with_team, when: required_filled }
  - { action: finish, from: [with_team, studying], to: done, when: required_filled }
on:
  with_team:
    - { do: copy, from: amount, to: proposed }
`

// TestStepsHandOver: each step gives the item to its owner (the starter, a team, a person field), a
// move with a condition is taken only when it is true, and copy keeps a value to compare with later.
func TestStepsHandOver(t *testing.T) {
	f := newFirm(t)
	p, err := playbook.Parse([]byte(handOff))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	f.tx(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(f.ctx, tx, p)
		return err
	})
	start := func(fields engine.Fields) workitem.Item {
		var it workitem.Item
		f.tx(func(tx pgx.Tx) error {
			it, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", Fields: fields,
				By: ledger.PersonActor(f.sales)})
			return err
		})
		return it
	}
	act := func(id, action string) (workitem.Item, error) {
		var it workitem.Item
		err := f.try(func(tx pgx.Tx) error {
			var err error
			it, err = workitem.Act(f.ctx, tx, id, action, "", ledger.PersonActor(f.sales))
			return err
		})
		return it, err
	}

	usual := start(engine.Fields{"who": "Acme", "kind": "usual", "amount": 500000.0})
	if usual.AssigneeID != f.sales || usual.Team != "" {
		t.Fatalf("new item with %q / team %q, want the starter", usual.AssigneeID, usual.Team)
	}
	it, err := act(usual.ID, "hand_over")
	if err != nil {
		t.Fatal(err)
	}
	if it.State != "with_team" || it.AssigneeID != "" || it.Team != "ops" {
		t.Errorf("after hand_over: %s, with %q / team %q; want with_team, the ops team", it.State, it.AssigneeID, it.Team)
	}
	if it.Fields["proposed"] != 500000.0 || it.Version != usual.Version+1 {
		t.Errorf("proposed = %v, version %d → %d; want the amount copied and a new version",
			it.Fields["proposed"], usual.Version, it.Version)
	}
	if it, err = act(usual.ID, "finish"); err != nil || it.State != "done" || it.Team != "ops" {
		t.Errorf("finish = %v, %s, team %q; want done, still with the team (done names no owner)", err, it.State, it.Team)
	}

	// Needs of the first step only: no amount is fine to hand over, but the team needs it to finish.
	novel := start(engine.Fields{"who": "Beta", "kind": "new", "lead": f.maker})
	if it, err = act(novel.ID, "hand_over"); err != nil || it.State != "studying" || it.AssigneeID != f.maker {
		t.Errorf("hand_over of new work = %v, %s with %q; want studying with the lead", err, it.State, it.AssigneeID)
	}
	third := start(engine.Fields{"who": "Gamma"})
	if _, err := act(third.ID, "hand_over"); err == nil {
		t.Error("hand_over without the kind was allowed")
	}

	f.tx(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}
