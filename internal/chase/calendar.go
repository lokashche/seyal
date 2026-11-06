// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"slices"
	"time"
)

// Calendar says which days a business is open (D75, decision 1): every day but its weekly days off
// and its holiday dates. Waits are counted in working days, by date in the business's time zone.
type Calendar struct {
	Zone     *time.Location // the business's time zone
	DaysOff  []time.Weekday // for example time.Sunday
	Holidays []string       // dates the office is closed, written 2026-11-09
}

// maxDays stops a count that could never end (the set-up file allows at least one working day a
// week, so it never comes near): about ten years.
const maxDays = 3660

// WorkingDays counts the working days after from's date, up to and including to's date, each date
// read in the calendar's time zone. A stay that started on Mon 30 Nov has waited 2 working days on
// Wed 2 Dec. It is 0 when to's date isn't after from's.
func (c Calendar) WorkingDays(from, to time.Time) int {
	day, last := c.date(from), c.date(to)
	n := 0
	for i := 0; i < maxDays && day.Before(last); i++ {
		day = day.AddDate(0, 0, 1)
		if working(day, c.DaysOff, c.Holidays) {
			n++
		}
	}
	return n
}

// IsWorkingDay reports whether t's date, read in the calendar's time zone, is a working day: not a
// weekly day off, not a holiday. Phone notes wait for one (D89).
func (c Calendar) IsWorkingDay(t time.Time) bool {
	return working(c.date(t), c.DaysOff, c.Holidays)
}

// HolidaysIn lists the holiday dates after from's date, up to and including to's date: the ones a
// count left out, for the ledger.
func (c Calendar) HolidaysIn(from, to time.Time) []string {
	first, last := c.date(from).Format(time.DateOnly), c.date(to).Format(time.DateOnly)
	out := []string{}
	for _, h := range c.Holidays {
		if h > first && h <= last {
			out = append(out, h)
		}
	}
	return out
}

// date is t's date in the calendar's time zone, as midnight in UTC: a plain date, so adding a day
// never meets a clock change.
func (c Calendar) date(t time.Time) time.Time {
	zone := c.Zone
	if zone == nil {
		zone = time.UTC
	}
	y, m, d := t.In(zone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// workingDaysAfter returns the date on which n working days have passed after the date start, without
// the weekly days off and the holidays: started Mon, 2 days: Wed. Started Fri, 2 days, Sunday off: Sat
// counts, Sun doesn't, so Mon. start is a date (any time of day; its own day is never counted).
func workingDaysAfter(start time.Time, n int, daysOff []time.Weekday, holidays []string) time.Time {
	y, m, d := start.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxDays && n > 0; i++ {
		day = day.AddDate(0, 0, 1)
		if working(day, daysOff, holidays) {
			n--
		}
	}
	return day
}

// working reports whether a date is a working day: not a weekly day off, not a holiday.
func working(day time.Time, daysOff []time.Weekday, holidays []string) bool {
	return !slices.Contains(daysOff, day.Weekday()) && !slices.Contains(holidays, day.Format(time.DateOnly))
}
