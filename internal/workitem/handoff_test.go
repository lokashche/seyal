// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"testing"
	"time"
)

// held_since is written once, in UTC, to whole microseconds, always with six digits, so the string in
// the snapshot equals the one SelectHeldSince reads back from Postgres.
func TestHeldSinceNow(t *testing.T) {
	got := heldSinceNow()
	at, err := time.Parse(HeldSinceLayout, got)
	if err != nil {
		t.Fatalf("%q doesn't read back: %v", got, err)
	}
	if len(got) != len("2026-11-05T09:40:00.000000Z") || got[len(got)-1] != 'Z' {
		t.Errorf("%q: want 6 digits after the seconds and a Z (UTC)", got)
	}
	if at.Nanosecond()%1000 != 0 {
		t.Errorf("%q keeps more than microseconds", got)
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("%q is not RFC 3339: %v", got, err)
	}
}
