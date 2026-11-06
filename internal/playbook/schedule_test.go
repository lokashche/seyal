// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

// scheduleExample is a made-up process that starts on its own (D75, R): every quarter, one item for
// each member of a list with a yes/no detail ticked.
const scheduleExample = `id: desk-review
version: "1"
description: A review done every quarter for each member who asked for one.
numbering: "R-{seq:4}"
title: "{member} — {period}"
roles: [team]
fields:
  - { key: member, label: Member, type: master, kind: member, required: true, prefill: { reviewer: looked_after_by } }
  - { key: period, label: Period, type: text, readonly: true }
  - { key: due_on, label: Due by, type: date }
  - { key: reviewer, label: Reviewer, type: person }
states: [open, done]
initial: open
steps:
  open: { owner: "field:reviewer", pass_to: ["role:team"] }
schedule:
  every: quarter
  day: 5
  for_each: member
  where: wants_review
  period: previous
  period_field: period
  due: { field: due_on, months_after: 1, day: last }
transitions:
  - { action: done, from: [open], to: done }
`

const memberKind = `kind: member
label: Member
fields:
  - { key: wants_review, label: Wants a review, type: bool }
  - { key: looked_after_by, label: Looked after by, type: person }
  - { key: note, label: Note, type: text }
`

func TestScheduleKeys(t *testing.T) {
	p, err := Parse([]byte(scheduleExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckNew(); err != nil {
		t.Fatalf("a good schedule is refused: %v", err)
	}
	s := p.Schedule
	if s == nil || s.Every != "quarter" || s.Day != 5 || s.Due.Day != "last" || s.Due.DueDay() != 0 || s.Limit() != DefaultMaxPerRun {
		t.Fatalf("schedule = %+v", s)
	}
	p2, _ := Parse([]byte(strings.Replace(scheduleExample, "day: last", "day: 20", 1)))
	if d := p2.Schedule.Due; d.Day != "20" || d.DueDay() != 20 {
		t.Errorf("due day 20 reads as %+v", d)
	}
	// The sample itself, and a process without a schedule, pass too.
	sample, err := LoadFile("../../examples/playbooks/monthly-return.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.CheckNew(); err != nil {
		t.Errorf("the sample monthly return is refused: %v", err)
	}
	if sample.Schedule.Limit() != 30 || !sample.Schedule.SameRuns(*sample.Schedule) {
		t.Errorf("sample schedule = %+v", sample.Schedule)
	}
}

func TestScheduleRefusals(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"every: quarter", "every: week", `every: use month, quarter or year, not "week"`},
		{"day: 5", "day: 29", "day: the run day must be from 1 to 28"},
		{"day: 5", "day: 0", "day: the run day must be from 1 to 28"},
		{"period: previous", "period: last", "period: use previous"},
		{"for_each: member", "for_each: period", `for_each: "period" must be a master field`},
		{"for_each: member", "for_each: nobody", `for_each: "nobody" must be a master field`},
		{"  where: wants_review\n", "", "where: name the yes/no detail"},
		{"period_field: period", "period_field: due_on", `period_field: "due_on" must be a text field`},
		{"field: due_on,", "field: period,", `due: field: "period" must be a date field`},
		{"day: last", "day: 31", `due: day: from 1 to 28, or last`},
		{"day: last", "day: end", `due: day: from 1 to 28, or last`},
		{"months_after: 1", "months_after: 13", "due: months_after: from 0 to 12"},
		{"  period_field: period\n", "  period_field: period\n  max_per_run: 501\n", "max_per_run: from 1 to 500"},
	} {
		p, err := Parse([]byte(strings.Replace(scheduleExample, c.from, c.to, 1)))
		if err != nil {
			t.Fatalf("%s: %v", c.to, err)
		}
		err = p.CheckNew()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.to, err, c.want)
		}
	}
	// A key that isn't a schedule key is a typo: refused when the file is read.
	if _, err := Parse([]byte(strings.Replace(scheduleExample, "day: 5", "day: 5\n  evry: month", 1))); err == nil {
		t.Error("an unknown schedule key is accepted")
	}
}

// The yes/no detail is checked with the business's lists: it must be there, and be yes/no. A list
// change that removes it is refused while the process is in use (checkKindUse uses the same check).
func TestScheduleNeedsTheDetail(t *testing.T) {
	p, err := Parse([]byte(scheduleExample))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ kind, want string }{
		{memberKind, ""},
		{strings.Replace(memberKind, "wants_review, label: Wants a review, type: bool", "wants_review, label: Wants a review, type: text", 1),
			`schedule: where: member has no yes/no detail "wants_review"`},
		{strings.Replace(memberKind, "  - { key: wants_review, label: Wants a review, type: bool }\n", "", 1),
			`schedule: where: member has no yes/no detail "wants_review"`},
	} {
		mk, err := ParseMasterKind([]byte(c.kind))
		if err != nil {
			t.Fatal(err)
		}
		err = (&Library{Masters: map[string]*MasterKind{"member": mk}}).Check(p)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("got %v, want %q", err, c.want)
		}
	}
}
