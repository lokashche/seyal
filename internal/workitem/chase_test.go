// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"testing"
	"time"
)

// TestStamps: a reminder or an escalation is never stored before the latest hand-over. An escalation
// that moves nothing is stored strictly after it, so it never reads as "Seyal handed it on", even when
// the hand-over came after the chaser read the time (or from a copy of the app whose clock runs ahead).
func TestStamps(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := Chase{Now: now}
	for _, tc := range []struct {
		name, held     string
		stamp, unmoved string
	}{
		{"never held", "", "2026-11-06T10:00:00.000000Z", "2026-11-06T10:00:00.000000Z"},
		{"held before the chaser read the time", "2026-11-06T09:00:00.000000Z",
			"2026-11-06T10:00:00.000000Z", "2026-11-06T10:00:00.000000Z"},
		{"handed over after it", "2026-11-06T10:00:05.000000Z",
			"2026-11-06T10:00:05.000000Z", "2026-11-06T10:00:05.000001Z"},
		{"handed over at the same moment", "2026-11-06T10:00:00.000000Z",
			"2026-11-06T10:00:00.000000Z", "2026-11-06T10:00:00.000001Z"},
	} {
		it := Item{HeldSince: tc.held}
		if got := c.stamp(it); got != tc.stamp {
			t.Errorf("%s: stamp = %s, want %s", tc.name, got, tc.stamp)
		}
		it.EscalatedAt = c.unmoved(it)
		if it.EscalatedAt != tc.unmoved || it.EscalatedTo() || it.HeldSince > it.EscalatedAt {
			t.Errorf("%s: unmoved = %s (escalated to: %v), want %s, not read as handed on, not before the hand-over",
				tc.name, it.EscalatedAt, it.EscalatedTo(), tc.unmoved)
		}
	}
}
