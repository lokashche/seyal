// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"errors"
	"slices"
	"testing"

	"seyal/internal/playbook"
)

// complete returns fields where every required field of the sample playbook is filled in.
func complete() Fields {
	return Fields{
		"client":          "acme",
		"nature_of_work":  "bookkeeping",
		"reason":          "Monthly books",
		"fee_agreed":      1200000.0,
		"integrity_check": "pass",
	}
}

func TestMissing(t *testing.T) {
	p := loadExample(t)
	want := []string{"client", "nature_of_work", "reason", "fee_agreed", "integrity_check"}
	if got := Missing(p, "draft", Fields{}); !slices.Equal(got, want) {
		t.Errorf("Missing(empty) = %v, want %v", got, want)
	}
	if got := Missing(p, "draft", complete()); len(got) != 0 {
		t.Errorf("Missing(complete) = %v, want none", got)
	}
}

func TestTransition(t *testing.T) {
	p := loadExample(t)
	partial := complete()
	delete(partial, "fee_agreed")

	tests := []struct {
		name, state, action, reason string
		fields                      Fields
		wantTo                      string
		wantErr                     error
	}{
		{"submit a complete draft", "draft", "submit", "", complete(), "pending_approval", nil},
		{"submit with a field missing", "draft", "submit", "", partial, "", &MissingError{}},
		{"submit when already approved", "approved", "submit", "", complete(), "", ErrNotNow},
		{"cancel without a reason", "draft", "cancel", " ", complete(), "", ErrReasonNeeded},
		{"cancel an approved item", "approved", "cancel", "Client withdrew", complete(), "cancelled", nil},
		{"cancel twice", "cancelled", "cancel", "again", complete(), "", ErrNotNow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			to, err := Transition(p, Item{State: tt.state, Fields: tt.fields}, tt.action, tt.reason, nil, false)
			switch want := tt.wantErr.(type) {
			case nil:
				if err != nil || to != tt.wantTo {
					t.Errorf("Transition = %q, %v; want %q", to, err, tt.wantTo)
				}
			case *MissingError:
				var missing *MissingError
				if !errors.As(err, &missing) || !slices.Equal(missing.Keys, []string{"fee_agreed"}) {
					t.Errorf("Transition error = %v, want still needed: fee_agreed", err)
				}
			default:
				if !errors.Is(err, want) {
					t.Errorf("Transition error = %v, want %v", err, want)
				}
			}
		})
	}

	if _, err := Transition(p, Item{State: "draft"}, "approve-everything", "", nil, false); err == nil {
		t.Error("Transition accepted an unknown action")
	}
}

func TestActionsAndClosed(t *testing.T) {
	p := loadExample(t)
	tests := []struct {
		state      string
		wantAction []string
		wantClosed bool
	}{
		{"draft", []string{"submit", "cancel"}, false},
		{"pending_approval", []string{"cancel"}, false},
		{"approved", []string{"cancel"}, false},
		{"cancelled", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := Actions(p, Item{State: tt.state}); !slices.Equal(got, tt.wantAction) {
				t.Errorf("Actions = %v, want %v", got, tt.wantAction)
			}
			if got := IsClosed(p, tt.state); got != tt.wantClosed {
				t.Errorf("IsClosed = %v, want %v", got, tt.wantClosed)
			}
		})
	}
}

func TestCheckSet(t *testing.T) {
	p := loadExample(t)
	draft := Item{State: "draft"}

	tests := []struct {
		name    string
		item    Item
		key     string
		value   any
		wantErr bool
	}{
		{"fee in minor units", draft, "fee_agreed", 1200000.0, false},
		{"fee with a fraction", draft, "fee_agreed", 12.5, true},
		{"fee as text", draft, "fee_agreed", "twelve", true},
		{"date in ISO form", draft, "client_data_due", "2026-11-15", false},
		{"date in another form", draft, "client_data_due", "15/11/2026", true},
		{"allowed choice", draft, "integrity_check", "fail", false},
		{"choice not in options", draft, "integrity_check", "maybe", true},
		{"list of text", draft, "required_documents", []any{"Bank statements", "Invoices"}, false},
		{"list with a number in it", draft, "required_documents", []any{"Bank statements", 3.0}, true},
		{"empty person", draft, "maker", "", true},
		{"clearing a field", draft, "advance", nil, false},
		{"readonly field", draft, "standard_fee", 100.0, true},
		{"unknown field", draft, "discount", 10.0, true},
		{"cancelled item", Item{State: "cancelled"}, "reason", "changed my mind", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSet(p, tt.item, tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckSet(%s = %v) error = %v, wantErr %v", tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}

// TestSteps: each step asks its own fields and needs its own; a conditional move is taken only when
// its condition is true, otherwise the default; owners are read from the step.
func TestSteps(t *testing.T) {
	p, err := playbook.Parse([]byte(`id: hand-off
version: "1"
description: Ask, hand over, maybe study.
numbering: "H-{seq:4}"
roles: [team]
fields:
  - { key: who, label: Who, type: text, required: true }
  - { key: kind, label: Kind, type: choice, options: [usual, new], required: true }
  - { key: amount, label: Amount, type: money }
  - { key: lead, label: Lead, type: person }
states: [asking, with_team, studying, done]
initial: asking
steps:
  asking: { owner: starter, asks: [who, kind, amount], needs: [who, kind] }
  with_team: { owner: "role:team", asks: [lead], needs: [amount] }
transitions:
  - { action: hand_over, from: [asking], to: studying, when: required_filled, if: { field: kind, eq: new } }
  - { action: hand_over, from: [asking], to: with_team, when: required_filled }
  - { action: finish, from: [with_team, studying], to: done, when: required_filled }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := ToAsk(p, "asking", Fields{"who": "A"}); !slices.Equal(got, []string{"kind", "amount"}) {
		t.Errorf("asking asks %v, want kind, amount (optional amount too)", got)
	}
	if got := Missing(p, "asking", Fields{"who": "A"}); !slices.Equal(got, []string{"kind"}) {
		t.Errorf("asking needs %v, want kind only", got)
	}
	if got := Missing(p, "studying", Fields{"amount": 5.0}); !slices.Equal(got, []string{"who", "kind"}) {
		t.Errorf("a state without a step needs %v, want the required fields", got)
	}
	if got := Actions(p, Item{State: "asking"}); !slices.Equal(got, []string{"hand_over"}) {
		t.Errorf("actions = %v, want hand_over once", got)
	}
	for _, tt := range []struct {
		kind, want string
	}{{"new", "studying"}, {"usual", "with_team"}} {
		to, err := Transition(p, Item{State: "asking", Fields: Fields{"who": "A", "kind": tt.kind}}, "hand_over", "", nil, false)
		if err != nil || to != tt.want {
			t.Errorf("hand_over with kind %s = %s, %v; want %s", tt.kind, to, err, tt.want)
		}
	}
	var missing *MissingError
	if _, err := Transition(p, Item{State: "with_team", Fields: Fields{"who": "A", "kind": "usual"}}, "finish", "", nil, false); !errors.As(err, &missing) ||
		!slices.Equal(missing.Keys, []string{"amount"}) {
		t.Errorf("finish without the amount = %v, want amount still needed", err)
	}
	if k, n := Owner(p, "with_team"); k != "role" || n != "team" {
		t.Errorf("owner of with_team = %s:%s", k, n)
	}
	if k, _ := Owner(p, "done"); k != "" {
		t.Errorf("owner of done = %q, want none (unchanged)", k)
	}
}

// TestSeyalOnlyMoves: a move marked "by: seyal" isn't offered to people and refuses them.
func TestSeyalOnlyMoves(t *testing.T) {
	p, err := playbook.Parse([]byte(`id: waits
version: "1"
description: Waits for linked work.
numbering: "W-{seq:4}"
fields:
  - { key: x, label: X, type: text }
states: [waiting, ready]
initial: waiting
transitions:
  - { action: linked_done, from: [waiting], to: ready, by: seyal }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := Actions(p, Item{State: "waiting"}); len(got) != 0 {
		t.Errorf("actions offered to people = %v, want none", got)
	}
	if _, err := Transition(p, Item{State: "waiting"}, "linked_done", "", nil, false); !errors.Is(err, ErrNotNow) {
		t.Errorf("a person took the move: %v", err)
	}
	if to, err := Transition(p, Item{State: "waiting"}, "linked_done", "", nil, true); err != nil || to != "ready" {
		t.Errorf("Seyal's move = %s, %v", to, err)
	}
	if IsClosed(p, "waiting") {
		t.Error("an item waiting for linked work counts as closed")
	}
}
