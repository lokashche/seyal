// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

// whoExample is a made-up desk process that uses every key added for moving work between people
// (D68): who, back, pass_to, returns_to, on_reject_back, copy if_empty and the filled condition.
const whoExample = `id: desk-request
version: "1"
description: A request the desk starts, a team handles and a manager approves.
numbering: "R-{seq:4}"
roles: [desk, team, manager]
fields:
  - { key: topic, label: Topic, type: text, required: true }
  - { key: amount, label: Amount, type: money }
  - { key: first_amount, label: First amount, type: money, readonly: true }
  - { key: findings, label: Findings, type: text, readonly: true }
  - { key: reviewer, label: Reviewer, type: person }
  - { key: size, label: Size, type: number }
states: [new, looking, with_team, waiting, approved, closed]
initial: new
steps:
  new: { owner: starter, returns_to: last }
  with_team: { owner: "role:team", pass_to: ["role:team"], returns_to: last }
  looking: { owner: "field:reviewer", pass_to: [anyone] }
transitions:
  - { action: send, from: [new], to: looking, who: [holder], if: { field: findings, filled: false } }
  - { action: send, from: [new], to: with_team, who: [holder] }
  - { action: found, from: [looking], to: with_team, who: [holder] }
  - { action: submit, from: [with_team], to: waiting, who: [holder] }
  - { action: ask_again, from: [with_team], to: new, needs: reason, back: true, who: [holder] }
  - { action: close, from: [new, with_team, waiting, approved], to: closed, needs: reason, who: [holder, starter, "role:manager"] }
approvals:
  in: waiting
  on_complete: approved
  on_reject: new
  on_reject_back: true
  slots:
    - { slot: manager, role: manager }
on:
  with_team:
    - { do: copy, from: amount, to: first_amount, if_empty: true }
`

func TestWhoKeys(t *testing.T) {
	p, err := Parse([]byte(whoExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("the example has problems: %v", err)
	}
	if err := p.CheckNew(); err != nil {
		t.Fatalf("the example can't be registered: %v", err)
	}
	if !p.Transitions[4].Back || p.Steps["new"].ReturnsTo != "last" || !p.Approvals.OnRejectBack ||
		!p.On["with_team"][0].IfEmpty || p.Transitions[0].If.Filled == nil || len(p.Steps["with_team"].PassTo) != 1 {
		t.Errorf("a new key wasn't read: %+v", p)
	}

	tests := []struct{ name, old, new, want string }{
		{"who: unknown role", `who: [holder, starter, "role:manager"]`, `who: [holder, starter, "role:nobody"]`, `unknown role "nobody"`},
		{"who: unknown word", "who: [holder] }\n  - { action: ask_again", "who: [boss] }\n  - { action: ask_again", `unknown word "boss"`},
		{"who: field not a person", "{ action: found, from: [looking], to: with_team, who: [holder] }",
			"{ action: found, from: [looking], to: with_team, who: [holder, \"field:topic\"] }", "needs a person field, topic is text"},
		{"who: unknown field", "{ action: found, from: [looking], to: with_team, who: [holder] }",
			"{ action: found, from: [looking], to: with_team, who: [holder, \"field:checker\"] }", `unknown field "checker"`},
		{"who: master sub-field", "{ action: found, from: [looking], to: with_team, who: [holder] }",
			"{ action: found, from: [looking], to: with_team, who: [holder, \"field:reviewer.name\"] }", "must name a person field"},
		{"who with by seyal", "{ action: submit, from: [with_team], to: waiting, who: [holder] }",
			"{ action: submit, from: [with_team], to: waiting, who: [holder], by: seyal }", `"who" doesn't go with "by: seyal"`},
		{"lines of one move differ in who", "{ action: send, from: [new], to: with_team, who: [holder] }",
			"{ action: send, from: [new], to: with_team, who: [starter] }", `same "who" and "back"`},
		{"lines of one move differ in back", "{ action: send, from: [new], to: with_team, who: [holder] }",
			"{ action: send, from: [new], to: with_team, who: [holder], back: true, needs: reason }", `same "who" and "back"`},
		{"back without a reason", "needs: reason, back: true", "back: true", `"back: true" needs "needs: reason"`},
		{"back into the approvals step", "to: new, needs: reason, back: true", "to: waiting, needs: reason, back: true", "the approvals step"},
		{"back into the approved step", "to: new, needs: reason, back: true", "to: approved, needs: reason, back: true", "the step after the approvals"},
		{"back into a closed step", "to: new, needs: reason, back: true", "to: closed, needs: reason, back: true", "no way out"},
		{"pass_to the holder", `pass_to: ["role:team"]`, "pass_to: [holder]", "holder can't receive the item"},
		{"pass_to an unknown role", `pass_to: ["role:team"]`, `pass_to: ["role:crew"]`, `unknown role "crew"`},
		{"returns_to something else", "owner: starter, returns_to: last", "owner: starter, returns_to: first", `unknown "returns_to" "first"`},
		{"if_empty on post", "{ do: copy, from: amount, to: first_amount, if_empty: true }",
			"{ do: post, text: hello, if_empty: true }", "if_empty only goes with copy"},
		{"filled with if_missing", "{ field: findings, filled: false }", "{ field: findings, filled: false, if_missing: true }", "if_missing doesn't go with filled"},
		{"filled and eq", "{ field: findings, filled: false }", "{ field: findings, filled: false, eq: x }", "exactly one of eq, ne, lt, gt, filled"},
		{"filled on an unknown field", "{ field: findings, filled: false }", "{ field: finding, filled: false }", `unknown field "finding"`},
		{"refusal sent back into the approvals step", "on_reject: new\n", "on_reject: waiting\n",
			"a refusal that sends work back can't go into waiting, the approvals step"},
		{"refusal sent back into the approved step", "on_reject: new\n", "on_reject: approved\n", "the step after the approvals"},
		{"refusal sent back into a closed step", "on_reject: new\n", "on_reject: closed\n", "closed, which has no way out"},
		{"unknown key", "on_reject_back: true", "on_reject_back: true\n  reject_back: true", "reject_back"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(whoExample, tt.old, tt.new, 1)
			if changed == whoExample {
				t.Fatalf("test setup: %q not found", tt.old)
			}
			p, err := Parse([]byte(changed))
			if err == nil {
				err = p.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v\nwant a problem mentioning %q", err, tt.want)
			}
		})
	}
}

// TestCheckNew: some checks run only when a version is registered for the first time, so a version a
// business already has keeps opening after an upgrade (D68, decision 24).
func TestCheckNew(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"a move named take", "action: submit,", "action: take,", `"take" is one of Seyal's own words`},
		{"a move named pass_on", "action: submit,", "action: pass_on,", `"pass_on" is one of Seyal's own words`},
		// Until the person field is protected (moving-work design, decision 14), field: in a move's
		// who would let anyone who may change the details name themselves and take the move.
		{"who: a person field", "{ action: found, from: [looking], to: with_team, who: [holder] }", "{ action: found, from: [looking], to: with_team, who: [holder, \"field:reviewer\"] }", `who: "field:reviewer" can't be used yet`},
		// The same for pass_to: the holder (or the starter) could set the field to anyone and the
		// holder could then pass the item to them as an ordinary pass (D71 review).
		{"pass_to: a person field", `pass_to: ["role:team"]`, `pass_to: ["role:team", "field:reviewer"]`,
			`step with_team: pass_to: "field:reviewer" can't be used yet`},
		{"the approvals step owned by a person field", "  looking: {", "  waiting: { owner: \"field:reviewer\" }\n  looking: {",
			"step waiting: owner: the approvals step"},
		{"the approved step owned by a person field", "  looking: {", "  approved: { owner: \"field:reviewer\" }\n  looking: {",
			"step approved: owner: the approvals step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(whoExample, tt.old, tt.new, 1)
			if changed == whoExample {
				t.Fatalf("test setup: %q not found", tt.old)
			}
			p, err := Parse([]byte(changed))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate (which opening a stored version runs) refused it: %v", err)
			}
			if err := p.CheckNew(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("CheckNew() = %v\nwant a problem mentioning %q", err, tt.want)
			}
		})
	}
}
