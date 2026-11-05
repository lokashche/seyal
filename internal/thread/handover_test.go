// SPDX-License-Identifier: AGPL-3.0-or-later

package thread

import (
	"testing"

	"seyal/internal/workitem"
)

// Each kind of hand-over reads as its own line (D70); events from before D70 have no "how" and read
// as they always did.
func TestHandOverLines(t *testing.T) {
	b := &builder{
		names: map[string]string{"p-ben": "Ben Example", "p-dev": "Dev Example", "p-asha": "Asha Example"},
		roles: map[string]string{"ops": "Ops"},
	}
	tests := []struct {
		name   string
		change map[string]any
		want   string
	}{
		{"old event to a person", map[string]any{"assignee_id": "p-asha"}, "assigned it to Asha Example"},
		{"old event to a team", map[string]any{"assignee_id": "", "team": "ops"}, "handed it to the Ops team"},
		{"a step's owner", map[string]any{"how": workitem.HowStep, "assignee_id": "", "team": "ops", "from_assignee": "p-asha"},
			"handed it to the Ops team"},
		{"an assign action", map[string]any{"how": workitem.HowRule, "assignee_id": "p-dev"}, "assigned it to Dev Example"},
		{"a pass with a note", map[string]any{"how": workitem.HowPass, "assignee_id": "p-dev", "note": "he knows them well"},
			"passed it to Dev Example: he knows them well"},
		{"a pass to the team", map[string]any{"how": workitem.HowPass, "assignee_id": "", "team": "ops"}, "passed it to the Ops team"},
		{"an admin's pass", map[string]any{"how": workitem.HowPass, "assignee_id": "p-dev", "as": "admin"},
			"passed it to Dev Example (as an admin)"},
		{"a take", map[string]any{"how": workitem.HowTake, "assignee_id": "p-ben", "from_team": "ops"}, "took it from the Ops team"},
		{"a take nobody held", map[string]any{"how": workitem.HowTake, "assignee_id": "p-ben"}, "took it"},
		// The reason is already in the move's own line, so it isn't repeated.
		{"a return", map[string]any{"how": workitem.HowReturn, "assignee_id": "p-asha", "note": "statement missing"},
			"gave it back to Asha Example"},
	}
	for _, tt := range tests {
		str := func(key string) string { s, _ := tt.change[key].(string); return s }
		if got := b.handOver(str); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}
