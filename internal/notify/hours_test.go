// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"testing"
	"time"

	"seyal/internal/chase"
	"seyal/internal/hours"
)

// TestHold: Kaveri Associates (demo) works 09:00-19:00 in Asia/Kolkata, with Sundays off. In November
// 2026, the 6th is a Friday and the 9th a Monday.
func TestHold(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, zone) }
	h := hours.Hours{From: 9 * 60, Until: 19 * 60}
	sundays := chase.Calendar{Zone: zone, DaysOff: []time.Weekday{time.Sunday}}
	holiday := chase.Calendar{Zone: zone, DaysOff: []time.Weekday{time.Sunday}, Holidays: []string{"2026-11-09"}}
	tests := []struct {
		name string
		at   time.Time
		cal  chase.Calendar
		want time.Time // zero: it goes now
	}{
		{"Friday 16:40 goes now", at(6, 16, 40), sundays, time.Time{}},
		{"09:00 sharp goes now", at(6, 9, 0), sundays, time.Time{}},
		{"18:59 goes now", at(6, 18, 59), sundays, time.Time{}},
		{"19:00 waits for Saturday morning", at(6, 19, 0), sundays, at(7, 9, 0)},
		{"Saturday evening waits over Sunday", at(7, 21, 30), sundays, at(9, 9, 0)},
		{"Sunday waits for Monday", at(8, 11, 0), sundays, at(9, 9, 0)},
		{"Monday 00:05 (the chaser) waits for 09:00", at(9, 0, 5), sundays, at(9, 9, 0)},
		{"a holiday Monday waits for Tuesday", at(9, 10, 0), holiday, at(10, 9, 0)},
		{"Saturday evening before a holiday Monday waits for Tuesday", at(7, 20, 0), holiday, at(10, 9, 0)},
		{"the same moment in UTC", at(9, 0, 5).UTC(), sundays, at(9, 9, 0)},
	}
	for _, tt := range tests {
		got := Hold(tt.at, h, tt.cal)
		if !got.Equal(tt.want) {
			t.Errorf("%s: Hold(%s) = %s, want %s", tt.name, tt.at.Format(time.RFC3339), got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
		}
	}
	allDay := hours.Hours{From: 0, Until: 24 * 60}
	if got := Hold(at(9, 23, 59), allDay, chase.Calendar{Zone: zone}); !got.IsZero() {
		t.Errorf("hours 00:00-24:00 hold a note at 23:59 until %s", got)
	}
	if got := Hold(at(8, 23, 59), allDay, sundays); !got.Equal(at(9, 0, 0)) {
		t.Errorf("hours 00:00-24:00 on a Sunday off hold until %s, want Monday 00:00", got)
	}
}
