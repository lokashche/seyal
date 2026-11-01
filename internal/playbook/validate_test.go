// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestValidateFindsMistakes(t *testing.T) {
	example, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, old, new, want string
	}{
		{"misspelled state", "to: pending_approval", "to: pending_aproval", `unknown state "pending_aproval"`},
		{"unknown initial state", "initial: draft", "initial: drafts", `initial: unknown state "drafts"`},
		{"unknown field type", "type: number }", "type: numbr }", `unknown type "numbr"`},
		{"duplicate field", "key: advance,", "key: reason,", `fields: duplicate "reason"`},
		{"undeclared role", "role: ops }", "role: operations }", `unknown role "operations"`},
		{"rule on unknown field", "field: fee_agreed,", "field: fees_agreed,", `unknown field "fees_agreed"`},
		{"master lookup on non-master", "nature_of_work.is_new", "reason.is_new", `"reason" is not a master field`},
		{"less-than on text", "field: fee_agreed, lt:", "field: reason, lt:", "number, money or date"},
		{"choice value not allowed", "eq: fail }", "eq: failed }", "not one of the options"},
		{"prefill into unknown field", "est_hours: est_hours", "est_hour: est_hours", `prefill: unknown field "est_hour"`},
		{"numbering without seq", `"J-{yyyy}-{seq:4}"`, `"J-{yyyy}"`, "{seq:N} exactly once"},
		{"title uses unknown field", "{client} —", "{customer} —", `title: unknown field "customer"`},
		{"deadline from non-date", "due_from: client_data_due", "due_from: reason", "must be a date field"},
		{"action on unknown state", "on:\n  approved:", "on:\n  aproved:", `unknown state "aproved"`},
		{"unknown action", "do: post,", "do: email,", `unknown action "email"`},
		{"assign to non-person", "to: maker }", "to: reason }", "assign needs a person field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(string(example), tt.old, tt.new, 1)
			if changed == string(example) {
				t.Fatalf("test setup: %q not found in the example", tt.old)
			}
			p, err := Parse([]byte(changed))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v\nwant a problem mentioning %q", err, tt.want)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	example, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.NewReplacer("initial: draft", "initial: drafts", "role: ops }", "role: operations }").
		Replace(string(example))
	p, err := Parse([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	var verr *ValidationError
	if err := p.Validate(); !errors.As(err, &verr) || len(verr.Problems) != 2 {
		t.Errorf("Validate() = %v, want exactly 2 problems", err)
	}
}

// stepsExample uses steps, conditional moves and copy.
const stepsExample = `id: hand-off
version: "1"
description: Ask, hand over to a team, maybe study first.
numbering: "H-{seq:4}"
roles: [front, team]
fields:
  - { key: who, label: Who, type: text, required: true, ask: "Who is it for?" }
  - { key: kind, label: Kind, type: choice, options: [usual, new], required: true }
  - { key: amount, label: Amount, type: money }
  - { key: proposed, label: Proposed amount, type: money, readonly: true }
  - { key: lead, label: Lead, type: person }
states: [asking, with_team, studying, done]
initial: asking
steps:
  asking: { owner: starter, asks: [who, kind, amount], needs: [who, kind] }
  with_team: { owner: "role:team", asks: [amount, lead], needs: [amount] }
  studying: { owner: "field:lead" }
transitions:
  - { action: hand_over, from: [asking], to: studying, when: required_filled, if: { field: kind, eq: new } }
  - { action: hand_over, from: [asking], to: with_team, when: required_filled }
  - { action: finish, from: [with_team, studying], to: done, when: required_filled }
on:
  with_team:
    - { do: copy, from: amount, to: proposed }
`

func TestValidateSteps(t *testing.T) {
	p, err := Parse([]byte(stepsExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("the steps example has problems: %v", err)
	}
	tests := []struct{ name, old, new, want string }{
		{"step on unknown state", "  studying: {", "  studyin: {", `step studyin: unknown state "studyin"`},
		{"owner role unknown", `"role:team"`, `"role:staff"`, `owner: unknown role "staff"`},
		{"owner field not a person", `"field:lead"`, `"field:who"`, "owner needs a person field"},
		{"owner of unknown form", "owner: starter", "owner: boss", `use starter, role:<role> or field:<person field>`},
		{"asks an unknown field", "asks: [amount, lead]", "asks: [amount, leader]", `asks: unknown field "leader"`},
		{"asks a read-only field", "asks: [amount, lead]", "asks: [amount, proposed]", "filled automatically"},
		{"needs an unknown field", "needs: [amount]", "needs: [amt]", `needs: unknown field "amt"`},
		{"default move first", "  - { action: hand_over, from: [asking], to: studying, when: required_filled, if: { field: kind, eq: new } }\n  - { action: hand_over, from: [asking], to: with_team, when: required_filled }",
			"  - { action: hand_over, from: [asking], to: with_team, when: required_filled }\n  - { action: hand_over, from: [asking], to: studying, when: required_filled, if: { field: kind, eq: new } }",
			"already has a move without a condition"},
		{"bad condition", "if: { field: kind, eq: new }", "if: { field: kind, eq: newer }", "not one of the options"},
		{"copy between types", "from: amount, to: proposed", "from: who, to: proposed", "the types must match"},
		{"copy from unknown field", "from: amount, to: proposed", "from: amt, to: proposed", `copy from: unknown field "amt"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(stepsExample, tt.old, tt.new, 1)
			if changed == stepsExample {
				t.Fatalf("test setup: %q not found", tt.old)
			}
			p, err := Parse([]byte(changed))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v\nwant a problem mentioning %q", err, tt.want)
			}
		})
	}
}
