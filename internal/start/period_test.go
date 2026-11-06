// SPDX-License-Identifier: AGPL-3.0-or-later

package start

import (
	"testing"
	"time"

	"seyal/internal/playbook"
)

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// The sample's schedule (examples/playbooks/monthly-return.yaml), and the same as a quarter or a year.
var (
	monthly   = playbook.Schedule{Every: "month", Day: 1, Period: "previous", Due: playbook.ScheduleDue{Field: "due_on", MonthsAfter: 1, Day: "20"}}
	quarterly = playbook.Schedule{Every: "quarter", Day: 1, Period: "previous", Due: playbook.ScheduleDue{Field: "due_on", MonthsAfter: 1, Day: "20"}}
	yearly    = playbook.Schedule{Every: "year", Day: 1, Period: "previous", Due: playbook.ScheduleDue{Field: "due_on", MonthsAfter: 1, Day: "20"}}
)

// Periods for months, quarters and years, with a financial year that starts in April (D75, R).
func TestDue(t *testing.T) {
	current := monthly
	current.Period = "current"
	fifteenth := monthly
	fifteenth.Day = 15
	noDue := monthly
	noDue.Due = playbook.ScheduleDue{}
	for _, c := range []struct {
		name               string
		s                  playbook.Schedule
		fiscal             int
		today              string
		period, key, runOn string
		dueOn              string
	}{
		{"month, on the run day", monthly, 4, "2026-12-01", "Nov 2026", "m2026-11", "2026-12-01", "2026-12-20"},
		{"month, the day before", monthly, 4, "2026-11-30", "Oct 2026", "m2026-10", "2026-11-01", "2026-11-20"},
		{"month, a late run keeps its due date", monthly, 4, "2026-12-22", "Nov 2026", "m2026-11", "2026-12-01", "2026-12-20"},
		{"month across a year", monthly, 4, "2027-01-01", "Dec 2026", "m2026-12", "2027-01-01", "2027-01-20"},
		{"month, current period", current, 4, "2026-11-06", "Nov 2026", "m2026-11", "2026-11-01", "2026-12-20"},
		{"month, run on the 15th", fifteenth, 4, "2026-12-10", "Oct 2026", "m2026-10", "2026-11-15", "2026-11-20"},
		{"month without a due date", noDue, 4, "2026-12-01", "Nov 2026", "m2026-11", "2026-12-01", ""},
		{"quarter, Jul–Sep", quarterly, 4, "2026-10-01", "Jul–Sep 2026", "q2026-07", "2026-10-01", "2026-10-20"},
		{"quarter, mid-quarter", quarterly, 4, "2026-11-06", "Jul–Sep 2026", "q2026-07", "2026-10-01", "2026-10-20"},
		{"quarter, the day before", quarterly, 4, "2026-09-30", "Apr–Jun 2026", "q2026-04", "2026-07-01", "2026-07-20"},
		{"quarter, Oct–Dec", quarterly, 4, "2027-01-05", "Oct–Dec 2026", "q2026-10", "2027-01-01", "2027-01-20"},
		{"quarter, Jan–Mar", quarterly, 4, "2027-04-01", "Jan–Mar 2027", "q2027-01", "2027-04-01", "2027-04-20"},
		{"quarter across a year", quarterly, 2, "2027-02-01", "Nov 2026–Jan 2027", "q2026-11", "2027-02-01", "2027-02-20"},
		{"year, April to March", yearly, 4, "2027-04-01", "2026-27", "y2026-04", "2027-04-01", "2027-04-20"},
		{"year, the day before", yearly, 4, "2027-03-31", "2025-26", "y2025-04", "2026-04-01", "2026-04-20"},
		{"calendar year", yearly, 1, "2027-01-01", "2026", "y2026-01", "2027-01-01", "2027-01-20"},
	} {
		r := Due(c.s, day(c.today), time.UTC, c.fiscal)
		if r.Name != c.period || r.Key != c.key || r.RunDay.Format(time.DateOnly) != c.runOn || r.DueOn != c.dueOn {
			t.Errorf("%s (%s): got %s %s, run %s, due %q; want %s %s, run %s, due %q", c.name, c.today,
				r.Name, r.Key, r.RunDay.Format(time.DateOnly), r.DueOn, c.period, c.key, c.runOn, c.dueOn)
		}
	}
}

// A late run on 22 Dec for Nov 2026 is due 20 Dec, so it is overdue as soon as it starts.
func TestLateRunIsOverdue(t *testing.T) {
	r := Due(monthly, day("2026-12-22"), time.UTC, 4)
	if r.Name != "Nov 2026" || !(r.DueOn < "2026-12-22") {
		t.Errorf("late run: %s due %s", r.Name, r.DueOn)
	}
}

// Due dates come from the period: February in a leap year and a normal one, 30-day months, and "last".
func TestDueDates(t *testing.T) {
	last := func(after int) playbook.Schedule {
		s := monthly
		s.Due = playbook.ScheduleDue{Field: "due_on", MonthsAfter: after, Day: "last"}
		return s
	}
	for _, c := range []struct {
		s            playbook.Schedule
		today, dueOn string
	}{
		{last(1), "2028-02-01", "2028-02-29"}, // Jan 2028 → the last day of Feb, a leap year
		{last(1), "2027-02-01", "2027-02-28"}, // Jan 2027 → a normal February
		{last(1), "2026-11-01", "2026-11-30"}, // Oct 2026 → November has 30 days
		{last(0), "2026-12-01", "2026-11-30"}, // Nov 2026 → its own last day
		{last(0), "2027-01-01", "2026-12-31"}, // Dec 2026 → 31 days
		{last(12), "2026-12-01", "2027-11-30"},
		{monthly, "2026-03-01", "2026-03-20"}, // Feb 2026 → 20 Mar
	} {
		if r := Due(c.s, day(c.today), time.UTC, 4); r.DueOn != c.dueOn {
			t.Errorf("%s (due %+v): due %s, want %s", c.today, c.s.Due, r.DueOn, c.dueOn)
		}
	}
	q := quarterly
	q.Due.Day = "last"
	if r := Due(q, day("2027-01-01"), time.UTC, 4); r.DueOn != "2027-01-31" {
		t.Errorf("Oct–Dec 2026, last day of the next month: %s", r.DueOn)
	}
}

// Today is the business's day: just after midnight in Asia/Kolkata on 1 Dec is still 30 Nov in UTC.
func TestDueInTheBusinessZone(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 30, 19, 0, 0, 0, time.UTC) // 1 Dec, 00:30 in India
	if r := Due(monthly, at, zone, 4); r.Name != "Nov 2026" {
		t.Errorf("got %s, want Nov 2026", r.Name)
	}
	if r := Due(monthly, at, time.UTC, 4); r.Name != "Oct 2026" {
		t.Errorf("in UTC got %s, want Oct 2026", r.Name)
	}
}

func TestNext(t *testing.T) {
	for _, c := range []struct {
		s           playbook.Schedule
		today, want string
	}{
		{monthly, "2026-12-01", "Dec 2026 2027-01-01"},
		{monthly, "2026-12-31", "Dec 2026 2027-01-01"},
		{quarterly, "2026-11-06", "Oct–Dec 2026 2027-01-01"},
		{yearly, "2026-11-06", "2026-27 2027-04-01"},
	} {
		n := Next(c.s, Due(c.s, day(c.today), time.UTC, 4), 4)
		if got := n.Name + " " + n.RunDay.Format(time.DateOnly); got != c.want {
			t.Errorf("next after %s: %s, want %s", c.today, got, c.want)
		}
	}
}

// The first run is the first run day on or after the day the schedule came into use: never a period
// before it. The day is the business's.
func TestCameIntoUse(t *testing.T) {
	zone, _ := time.LoadLocation("Asia/Kolkata")
	inUse := time.Date(2026, 11, 6, 9, 0, 0, 0, zone)
	for _, c := range []struct {
		runDay string
		since  time.Time
		want   bool
	}{
		{"2026-11-01", inUse, false}, // the Oct 2026 run: before it was in use
		{"2026-12-01", inUse, true},  // the Nov 2026 run
		{"2026-11-06", inUse, true},  // a run on the same day counts
		{"2026-12-01", time.Time{}, false},
		{"2026-12-01", time.Date(2026, 11, 30, 19, 0, 0, 0, time.UTC), true}, // 1 Dec, 00:30 in India
		{"2026-12-01", time.Date(2026, 12, 1, 19, 0, 0, 0, time.UTC), false}, // switched on again on 2 Dec
	} {
		if got := cameIntoUse(Period{RunDay: day(c.runDay)}, c.since, zone); got != c.want {
			t.Errorf("run %s, in use since %v: %v, want %v", c.runDay, c.since, got, c.want)
		}
	}
}
