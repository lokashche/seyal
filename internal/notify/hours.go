// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"time"

	"seyal/internal/chase"
	"seyal/internal/hours"
)

// Quiet hours (design section 2.1, decision 3): Seyal buzzes a phone only in the business's working
// hours (internal/hours) on its working days. A phone note made at any other time waits for the next
// working morning, and then goes as one morning note.

// Hold says when a phone note made at t may go: the zero time when t is inside working hours on a
// working day (it goes now), otherwise the start of working hours on the next working day, in the
// calendar's time zone. A reminder the chaser writes at 00:05 waits until 09:00; one made on Saturday
// evening, with Sunday off, waits until Monday 09:00.
func Hold(t time.Time, h hours.Hours, cal chase.Calendar) time.Time {
	zone := cal.Zone
	if zone == nil {
		zone = time.UTC
	}
	local := t.In(zone)
	y, m, d := local.Date()
	for i := range maxDays {
		day := time.Date(y, m, d+i, 12, 0, 0, 0, zone) // noon: never a clock change
		if !cal.IsWorkingDay(day) {
			continue
		}
		dy, dm, dd := day.Date()
		start := time.Date(dy, dm, dd, 0, h.From, 0, 0, zone)
		end := time.Date(dy, dm, dd, 0, h.Until, 0, 0, zone)
		if i == 0 && !local.Before(start) && local.Before(end) {
			return time.Time{}
		}
		if start.After(local) {
			return start
		}
	}
	return time.Time{} // never: the set-up file keeps at least one working day a week
}

// maxDays is how far Hold looks for a working day. The set-up file keeps at least one working day a
// week, but a long list of holidays could cover several weeks.
const maxDays = 400
