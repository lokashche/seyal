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
