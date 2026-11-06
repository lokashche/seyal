// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"fmt"
	"slices"
	"strconv"
)

// Schedule is a process's "start on its own" rule (D75, R): every month, quarter or year, on one day,
// Seyal starts one item for each active entry of a list that has a yes/no detail ticked. The engine
// only reads the keys; which list and which detail are named in the process file.
type Schedule struct {
	Every       string      `yaml:"every"`        // month, quarter or year (quarters and years follow the financial year)
	Day         int         `yaml:"day"`          // the run day of the month, 1 to 28
	ForEach     string      `yaml:"for_each"`     // a master field: one item for each active entry of its list...
	Where       string      `yaml:"where"`        // ...that has this yes/no detail ticked (required)
	Period      string      `yaml:"period"`       // previous (the period that just ended) or current (the one starting)
	PeriodField string      `yaml:"period_field"` // optional: a text field that gets the period's name ("Nov 2026")
	Due         ScheduleDue `yaml:"due"`          // optional: a date field set from the period
	MaxPerRun   int         `yaml:"max_per_run"`  // optional: more matches than this and the run starts none (0: DefaultMaxPerRun)
}

// ScheduleDue sets a date field from the period, not from the run day: Day of the month MonthsAfter
// months after the period's last month. { months_after: 1, day: 20 } for Nov 2026 is 20 Dec 2026.
type ScheduleDue struct {
	Field       string `yaml:"field"`        // the date field
	MonthsAfter int    `yaml:"months_after"` // 0 to 12; 0 is the period's own last month
	Day         string `yaml:"day"`          // 1 to 28, or last (the month's last day)
}

const (
	DefaultMaxPerRun = 30  // a run with more matches than this starts none, unless the file says otherwise
	MaxMaxPerRun     = 500 // the highest max_per_run a file may give
)

// Every word a schedule may use.
var (
	scheduleEvery  = []string{"month", "quarter", "year"}
	schedulePeriod = []string{"previous", "current"}
)

// Limit is the most entries one run may start.
func (s Schedule) Limit() int {
	if s.MaxPerRun > 0 {
		return s.MaxPerRun
	}
	return DefaultMaxPerRun
}

// SameRuns reports whether two schedules have the same runs: the same periods and run days, over the
// same list. Only a change to these makes a new version's schedule start afresh (it then never starts a
// period before it came into use); a new limit, due date or detail keeps the runs where they were, so a
// run held back by the limit still starts once the limit is raised.
func (s Schedule) SameRuns(o Schedule) bool {
	return s.Every == o.Every && s.Day == o.Day && s.Period == o.Period && s.ForEach == o.ForEach
}

// DueDay reads due.day: the day of the month, or 0 for the month's last day.
func (d ScheduleDue) DueDay() int {
	n, _ := strconv.Atoi(d.Day)
	return n
}

// scheduleProblems checks the schedule (D75, R). It is new in Phase 2, so it runs only for a version
// being added (see CheckNew). Whether the list has the yes/no detail is checked with the lists
// (checkMasterUse).
func (p *Playbook) scheduleProblems() []string {
	s := p.Schedule
	if s == nil {
		return nil
	}
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("schedule: "+format, args...))
	}
	fields := map[string]Field{}
	for _, f := range p.Fields {
		fields[f.Key] = f
	}
	if !slices.Contains(scheduleEvery, s.Every) {
		add("every: use month, quarter or year, not %q", s.Every)
	}
	if s.Day < 1 || s.Day > 28 {
		add("day: the run day must be from 1 to 28 (every month has those days), not %d", s.Day)
	}
	if !slices.Contains(schedulePeriod, s.Period) {
		add("period: use previous (the period that just ended) or current (the one just starting), not %q", s.Period)
	}
	switch f, ok := fields[s.ForEach]; {
	case s.ForEach == "":
		add("for_each: name the list field that gets one item per entry")
	case !ok || f.Type != "master":
		add("for_each: %q must be a master field of this process (type: master)", s.ForEach)
	}
	if s.Where == "" {
		add("where: name the yes/no detail of the list that picks the entries (required, so a mistake can't start an item for every entry)")
	} else if !keyPattern.MatchString(s.Where) {
		add("where: %q must use lowercase letters, digits and _", s.Where)
	}
	if s.PeriodField != "" {
		if f, ok := fields[s.PeriodField]; !ok || f.Type != "text" {
			add("period_field: %q must be a text field of this process", s.PeriodField)
		}
	}
	if d := s.Due; d != (ScheduleDue{}) {
		if f, ok := fields[d.Field]; !ok || f.Type != "date" {
			add("due: field: %q must be a date field of this process", d.Field)
		}
		if n := d.DueDay(); d.Day != "last" && (n < 1 || n > 28 || strconv.Itoa(n) != d.Day) {
			add("due: day: from 1 to 28, or last (the month's last day), not %q", d.Day)
		}
		if d.MonthsAfter < 0 || d.MonthsAfter > 12 {
			add("due: months_after: from 0 to 12, not %d", d.MonthsAfter)
		}
	}
	if s.MaxPerRun < 0 || s.MaxPerRun > MaxMaxPerRun {
		add("max_per_run: from 1 to %d, not %d", MaxMaxPerRun, s.MaxPerRun)
	}
	return problems
}
