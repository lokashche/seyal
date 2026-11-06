// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

// timingExample is a made-up process that uses the timing keys (D75, M6): a team step that reminds and
// escalates, a step owned by a person field that only reminds, and an approvals step that only
// reminds.
const timingExample = `id: desk-timing
version: "1"
description: A request a team handles, a checker checks and a manager approves.
numbering: "T-{seq:4}"
roles: [team, manager]
fields:
  - { key: topic, label: Topic, type: text, required: true }
  - { key: checker, label: Checker, type: person }
states: [with_team, checking, waiting, closed]
initial: with_team
steps:
  with_team:
    owner: "role:team"
    remind_after: 2 days
    escalate_after: 4 days
    escalate_to: "role:manager"
  checking:
    owner: "field:checker"
    remind_after: 1 day
  waiting:
    remind_after: 1 day
transitions:
  - { action: check, from: [with_team], to: checking }
  - { action: submit, from: [checking], to: waiting }
  - { action: close, from: [with_team, checking, waiting], to: closed, needs: reason }
approvals:
  in: waiting
  on_complete: closed
  on_reject: with_team
  slots:
    - { slot: manager, role: manager }
`

func TestDays(t *testing.T) {
	for in, want := range map[string]int{"1 day": 1, "2 days": 2, "60 days": 60, " 3 days ": 3} {
		if n, err := Days(in); err != nil || n != want {
			t.Errorf("Days(%q) = %d, %v; want %d", in, n, err, want)
		}
	}
	for _, in := range []string{"", "2", "1 days", "2 day", "0 days", "61 days", "2 hours", "two days", "2  days", "-1 days"} {
		if n, err := Days(in); err == nil {
			t.Errorf("Days(%q) = %d; want it refused", in, n)
		}
	}
}

func TestTimingKeys(t *testing.T) {
	p, err := Parse([]byte(timingExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("the example has problems: %v", err)
	}
	if err := p.CheckNew(); err != nil {
		t.Fatalf("the example can't be registered: %v", err)
	}
	if r, e := p.Steps["with_team"].Timing(); r != 2 || e != 4 {
		t.Errorf("with_team timing = %d, %d; want 2, 4", r, e)
	}
}

// TestTimingRefused: the checks a new version gets for its timing. Stored versions are never checked
// again (D68 choice 24), so these run only in CheckNew; Validate still accepts the file.
func TestTimingRefused(t *testing.T) {
	for _, tc := range []struct{ name, old, new, want string }{
		{"escalation in a step owned by a person field", "    remind_after: 1 day\n  waiting:",
			"    remind_after: 1 day\n    escalate_after: 3 days\n    escalate_to: \"role:manager\"\n  waiting:",
			"step checking: no escalation in a step owned by a person field (checker)"},
		{"escalation to the step's own team", `escalate_to: "role:manager"`, `escalate_to: "role:team"`,
			"step with_team: escalate_to can't be the step's own team (role:team)"},
		{"escalation in the approvals step", "  waiting:\n    remind_after: 1 day",
			"  waiting:\n    remind_after: 1 day\n    escalate_after: 2 days\n    escalate_to: \"role:manager\"",
			"step waiting: no escalation in the approvals step"},
		{"escalation not after the reminder", "escalate_after: 4 days", "escalate_after: 2 days",
			"step with_team: escalate_after (2 days) must be longer than remind_after (2 days)"},
		{"escalation without a reminder", "    remind_after: 2 days\n    escalate_after", "    escalate_after",
			"step with_team: escalate_after and escalate_to need remind_after"},
		{"escalation without a receiver", "    escalate_to: \"role:manager\"\n", "",
			"step with_team: escalate_after needs escalate_to"},
		{"a receiver without escalation", "    escalate_after: 4 days\n", "", "step with_team: escalate_to needs escalate_after"},
		{"an unknown role", `escalate_to: "role:manager"`, `escalate_to: "role:boss"`, `escalate_to: unknown role "boss"`},
		{"not a person field", `escalate_to: "role:manager"`, `escalate_to: "field:topic"`, `"field:topic" must name a person field`},
		{"neither role nor field", `escalate_to: "role:manager"`, `escalate_to: "manager"`, `escalate_to "manager": use role:<role> or field:<person field>`},
		{"hours", "remind_after: 2 days", "remind_after: 4 hours", `remind_after: "4 hours": write it as "1 day" or "<n> days"`},
		{"too long", "escalate_after: 4 days", "escalate_after: 90 days", `"90 days": from 1 to 60 days`},
		{"timing on a closed step", "  waiting:\n    remind_after: 1 day", "  waiting:\n    remind_after: 1 day\n  closed:\n    remind_after: 1 day",
			"step closed: has no way out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(timingExample, tc.old) {
				t.Fatalf("the example has no %q", tc.old)
			}
			p, err := Parse([]byte(strings.Replace(timingExample, tc.old, tc.new, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate = %v; the timing checks belong in CheckNew only", err)
			}
			err = p.CheckNew()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("CheckNew = %v; want %q", err, tc.want)
			}
		})
	}
}
