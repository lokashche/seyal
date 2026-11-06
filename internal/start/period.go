// SPDX-License-Identifier: AGPL-3.0-or-later

// Package start is the "schedule" way to start work (D75, R): a process file's schedule starts one item
// for each ticked list entry, every month, quarter or year. Due works out which run is due on a day
// (pure date maths); Find and Run, in run.go, look for what is due and do it, one step at a time, for
// the chaser. Starting from a message ("chat") stays in the server and ask packages, and linked work
// ("event") in workitem; there is no shared interface for the three ways yet (D75, decision 19).
package start

import (
	"fmt"
	"time"

	"seyal/internal/playbook"
)

// A Period is one run of a schedule: the period it covers and the day it happens.
type Period struct {
	RunDay time.Time // the run day: a date (midnight in UTC)
	First  time.Time // the period's first day: a date (midnight in UTC)
	Months int       // how long the period is: 1, 3 or 12
	Name   string    // what people call the period: "Nov 2026", "Jul–Sep 2026", "2026-27"
	Key    string    // the period in the key that stops repeats: "m2026-11", "q2026-07", "y2026-04"
	DueOn  string    // the due date the schedule gives ("2026-12-20"), or "" without one
}

// Due returns the run of s that is due on today: the latest run whose day is on or before today's date
// in zone. A run is caught up until the next run's day (D75, decision 5b), so it is the only one that
// can still start. fiscalStart is the business's first month of the year (4 for April): quarters and
// years follow it.
func Due(s playbook.Schedule, today time.Time, zone *time.Location, fiscalStart int) Period {
	if zone == nil {
		zone = time.UTC
	}
	y, m, d := today.In(zone).Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	n := length(s.Every)
	// The period that today's month is in, then back one period at a time until its run day has come.
	// With "previous" the run of the period today is in comes after today, so this takes two steps at most.
	first := monthIndex(y, m)
	first -= mod(first-align(s.Every, fiscalStart), n)
	for i := 0; i < 4; i++ {
		r := runOf(s, first-i*n, fiscalStart)
		if !r.RunDay.After(day) {
			return r
		}
	}
	return runOf(s, first-4*n, fiscalStart) // not reached: a run day is never more than a year after its period
}

// Next returns the run after r: the next period's.
func Next(s playbook.Schedule, r Period, fiscalStart int) Period {
	return runOf(s, monthIndex(r.First.Year(), r.First.Month())+r.Months, fiscalStart)
}

// runOf works out the run of the period that starts in month first (see monthIndex).
func runOf(s playbook.Schedule, first, fiscalStart int) Period {
	n := length(s.Every)
	runMonth := first + n // previous: the month after the period ends
	if s.Period == "current" {
		runMonth = first // current: the period's own first month
	}
	r := Period{RunDay: date(runMonth, s.Day), First: date(first, 1), Months: n}
	last := date(first+n-1, 1)
	switch n {
	case 1:
		r.Name, r.Key = r.First.Format("Jan 2006"), "m"+r.First.Format("2006-01")
	case 3:
		r.Name, r.Key = r.First.Format("Jan")+"–"+last.Format("Jan 2006"), "q"+r.First.Format("2006-01")
		if r.First.Year() != last.Year() {
			r.Name = r.First.Format("Jan 2006") + "–" + last.Format("Jan 2006")
		}
	default:
		r.Name, r.Key = fmt.Sprintf("%d-%02d", r.First.Year(), last.Year()%100), "y"+r.First.Format("2006-01")
		if fiscalStart <= 1 {
			r.Name = r.First.Format("2006") // a year from January is just its number
		}
	}
	if d := s.Due; d.Field != "" {
		month := first + n - 1 + d.MonthsAfter
		day := d.DueDay()
		if d.Day == "last" {
			day = date(month+1, 1).AddDate(0, 0, -1).Day()
		}
		r.DueOn = date(month, day).Format(time.DateOnly)
	}
	return r
}

// length is how many months a period has.
func length(every string) int {
	switch every {
	case "quarter":
		return 3
	case "year":
		return 12
	}
	return 1
}

// align is a month (as a month index) where a period starts: quarters and years start in the financial
// year's first month.
func align(every string, fiscalStart int) int {
	if every == "month" || fiscalStart < 1 || fiscalStart > 12 {
		return 0
	}
	return fiscalStart - 1
}

// monthIndex counts months from year 0, so going back or on a month is subtracting or adding 1.
func monthIndex(y int, m time.Month) int { return y*12 + int(m) - 1 }

// date is the day of the month with the given month index, as a plain date (midnight in UTC).
func date(month, day int) time.Time {
	return time.Date(month/12, time.Month(month%12+1), day, 0, 0, 0, 0, time.UTC)
}

// mod is a remainder that is never negative.
func mod(a, n int) int { return (a%n + n) % n }
