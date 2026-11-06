// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hours reads a business's working hours for phone notes, written "09:00-19:00" (D89). It is
// a small package on its own, using only the standard library, so both the set-up file (which checks
// the hours) and the phone-note reader (which waits for them) can use it without importing each other.
package hours

import (
	"fmt"
	"strconv"
	"strings"
)

// Default are the working hours when the set-up file doesn't say (notify_hours).
const Default = "09:00-19:00"

// Hours are working hours, in minutes after midnight in the business's time zone. Until may be
// 24*60 ("24:00": until midnight).
type Hours struct{ From, Until int }

// Parse reads hours written as "09:00-19:00". From must come before Until, and both are on the same
// day: a night stretch ("22:00-06:00") isn't allowed.
func Parse(s string) (Hours, error) {
	from, until, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Hours{}, fmt.Errorf("notify_hours %q: write it as 09:00-19:00", s)
	}
	f, err1 := clock(from, false)
	u, err2 := clock(until, true)
	if err1 != nil || err2 != nil {
		return Hours{}, fmt.Errorf("notify_hours %q: write it as 09:00-19:00 (24-hour times)", s)
	}
	if f >= u {
		return Hours{}, fmt.Errorf("notify_hours %q: the start must be before the end, on the same day", s)
	}
	return Hours{From: f, Until: u}, nil
}

// clock reads "09:00" as minutes after midnight; "24:00" only when end is true.
func clock(s string, end bool) (int, error) {
	s = strings.TrimSpace(s)
	if end && s == "24:00" {
		return 24 * 60, nil
	}
	hh, mm, ok := strings.Cut(s, ":")
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if !ok || len(hh) != 2 || len(mm) != 2 || err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q is not a time", s)
	}
	return h*60 + m, nil
}

// FromText and UntilText write the hours as the database keeps them: "09:00", "19:00".
func (h Hours) FromText() string  { return hhmm(h.From) }
func (h Hours) UntilText() string { return hhmm(h.Until) }

// String writes the hours as the set-up file does: "09:00-19:00".
func (h Hours) String() string { return h.FromText() + "-" + h.UntilText() }

func hhmm(minutes int) string { return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60) }
