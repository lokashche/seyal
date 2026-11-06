// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import "testing"

// TestIsNew: an item is New for a person when it arrived (a hand-over, or a new step stay) after they
// last saw it, or they never saw it; not when they handed it over themselves, and never for an old item
// with neither time (D75, M7).
func TestIsNew(t *testing.T) {
	const (
		dev   = "dev-id"
		ben   = "ben-id"
		early = "2026-12-02T09:40:00.000000Z"
		later = "2026-12-02T10:00:00.000000Z"
	)
	for _, c := range []struct {
		name string
		it   Item
		seen string
		want bool
	}{
		{"passed to him, never opened", Item{HeldSince: early, HandoffFrom: ben}, "", true},
		{"passed to him after he last looked", Item{HeldSince: later, HandoffFrom: ben}, early, true},
		{"opened after it came", Item{HeldSince: early, HandoffFrom: ben}, later, false},
		{"opened at the same moment", Item{HeldSince: early}, early, false},
		{"a new step stay after he looked (for example approvals reset)", Item{HeldSince: early, StepSince: later}, early, true},
		{"he passed it to his own team", Item{HeldSince: later, HandoffFrom: dev, Team: "ops"}, early, false},
		{"he passed it on, then someone moved it to a new step (his approval)", Item{HeldSince: early, StepSince: later, HandoffFrom: dev}, early, true},
		{"he passed it on, and the step began with that hand-over", Item{HeldSince: later, StepSince: later, HandoffFrom: dev}, early, false},
		{"he passed it on in a step that began earlier", Item{HeldSince: later, StepSince: early, HandoffFrom: dev}, "", false},
		{"he passed it on, and looked after the new step", Item{HeldSince: early, StepSince: later, HandoffFrom: dev}, later, false},
		{"an old item with no times", Item{}, "", false},
	} {
		if got := IsNew(c.it, dev, c.seen); got != c.want {
			t.Errorf("%s: IsNew = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTidyTitle: a title pattern whose last (or first) field is still empty doesn't end (or start) with
// its separator (D80). Other marks inside the title stay.
func TestTidyTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Deccan Motors —":           "Deccan Motors",
		"— Nov 2026":                "Nov 2026",
		"Deccan Motors — Nov 2026":  "Deccan Motors — Nov 2026",
		"Acme Traders: ":            "Acme Traders",
		" — ":                       "",
		"Annual accounts (2026-27)": "Annual accounts (2026-27)",
	} {
		if got := tidyTitle(in); got != want {
			t.Errorf("tidyTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
