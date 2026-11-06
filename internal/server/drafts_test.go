// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"slices"
	"strings"
	"testing"

	"seyal/internal/playbook"
)

// A draft's "What changes" shows a schedule being added, removed or changed (D75, R), so an admin sees
// before saving that a version starts work on its own, and how much of it.
func TestDraftChangesShowTheSchedule(t *testing.T) {
	sample, err := playbook.LoadFile("../../examples/playbooks/monthly-return.yaml")
	if err != nil {
		t.Fatal(err)
	}
	parse := func(from, to string) *playbook.Playbook {
		t.Helper()
		text := strings.Replace(string(sample.Source), from, to, 1)
		if text == string(sample.Source) {
			t.Fatalf("the sample has no %q", from)
		}
		p, err := playbook.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	view := func(p *playbook.Playbook) *processView {
		return &processView{ScheduleRule: describer{p: p}.schedule()}
	}
	rule := "Starts on its own: Monthly on the 1st, for the month just ended: for clients with “monthly return” ticked, at most 30 a run." +
		" Fills Period with the period's name. Due by: the 20th of the month after the period."
	if got := plainLines(view(sample)); !slices.Contains(got, rule) {
		t.Errorf("the sample's lines miss %q:\n%s", rule, strings.Join(got, "\n"))
	}
	i := strings.Index(string(sample.Source), "schedule:")
	without, err := playbook.Parse([]byte(string(sample.Source)[:i]))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		after *playbook.Playbook
		want  string
	}{
		{"a higher limit", parse("max_per_run: 30", "max_per_run: 500"), "at most 500 a run"},
		{"another detail", parse("where: monthly_return", "where: is_new"), "with “is new” ticked"},
		{"the period starting", parse("period: previous", "period: current"), "for the month just starting"},
		{"another day", parse("  day: 1   ", "  day: 5   "), "Monthly on the 5th"},
		{"quarterly", parse("every: month ", "every: quarter "), "Quarterly on the 1st, for the quarter just ended"},
		{"another list field", parse("for_each: client ", "for_each: preparer "), "for preparers with"},
		{"no schedule", without, ""},
	} {
		got := changes(view(sample), view(c.after))
		if !slices.Contains(got, "− "+rule) {
			t.Errorf("%s: what changes doesn't show the old schedule going: %q", c.name, got)
		}
		if c.want != "" && !slices.ContainsFunc(got, func(l string) bool {
			return strings.HasPrefix(l, "+ Starts on its own: ") && strings.Contains(l, c.want)
		}) {
			t.Errorf("%s: what changes doesn't show %q: %q", c.name, c.want, got)
		}
	}
	if got := changes(view(without), view(sample)); !slices.Contains(got, "+ "+rule) {
		t.Errorf("adding a schedule: what changes is %q", got)
	}
}
