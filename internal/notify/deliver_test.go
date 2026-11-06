// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestCompose: what one person's phone note says, for the notices that go for them in one pass.
func TestCompose(t *testing.T) {
	morning := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	note := func(id int64, kind, item, number, from, batch string, held bool) waiting {
		w := waiting{ID: id, Person: dev, Kind: kind, Item: item, Number: number, From: from, Batch: batch}
		if held {
			w.Hold = &morning
		}
		return w
	}
	n7 := func(id int64, kind string) waiting { return note(id, kind, "item-7", "J-2026-0007", ben, "", false) }
	tests := []struct {
		name     string
		list     []waiting
		business string
		want     string
		item     string // "" means it opens My Work
		ttl      time.Duration
	}{
		{"one hand-over", []waiting{n7(1, NewWork)}, "", "New work from Ben: J-2026-0007", "item-7", TTL},
		{"one, for a person in two businesses", []waiting{n7(1, NewWork)}, "Kaveri Associates (demo)",
			"Kaveri Associates (demo) · New work from Ben: J-2026-0007", "item-7", TTL},
		{"a team hand-over", []waiting{{ID: 1, Kind: TeamWork, Item: "item-7", Number: "J-2026-0007", From: ben, Team: "ops"}}, "",
			"New work for Operations from Ben: J-2026-0007", "item-7", TTL},
		{"two about one item", []waiting{n7(1, NewWork), n7(2, Approve)}, "", "2 new things for you", "item-7", TTL},
		{"three about three items", []waiting{n7(1, NewWork), note(2, Reminder, "item-8", "J-2026-0008", "", "", false),
			note(3, Approve, "item-9", "J-2026-0009", asha, "", false)}, "", "3 new things for you", "", TTL},
		{"a bulk move", []waiting{note(1, NewWork, "item-7", "J-2026-0007", chitra, "b1", false),
			note(2, NewWork, "item-8", "J-2026-0008", chitra, "b1", false)}, "", "2 items moved to you by Chitra", "", TTL},
		{"a bulk move and something else", []waiting{note(1, NewWork, "item-7", "J-2026-0007", chitra, "b1", false),
			note(2, Reminder, "item-8", "J-2026-0008", "", "", false)}, "", "2 new things for you", "", TTL},
		{"one held note keeps its own words", []waiting{note(1, Reminder, "item-7", "J-2026-0007", "", "", true)}, "",
			"Reminder: J-2026-0007 is waiting on you", "item-7", MorningTTL},
		{"the morning note", []waiting{note(1, Reminder, "item-7", "J-2026-0007", "", "", true),
			note(2, Reminder, "item-8", "J-2026-0008", "", "", true), note(3, NewWork, "item-9", "J-2026-0009", "", "", true)}, "",
			"While it was quiet: 2 reminders, 1 new work", "", MorningTTL},
	}
	for _, tt := range tests {
		n := compose(tt.list, names, teams, tt.business)
		if n.Body != tt.want || n.item != tt.item || n.TTL != tt.ttl || n.Urgency != "normal" || len(n.Notices) != len(tt.list) {
			t.Errorf("%s: %q (item %q, TTL %s, %d notices); want %q (item %q, TTL %s, %d)", tt.name, n.Body, n.item, n.TTL,
				len(n.Notices), tt.want, tt.item, tt.ttl, len(tt.list))
		}
	}
}

// TestTag: the Topic is 32 letters, digits, "-" and "_" (RFC 8030), never the item's number, and the
// same for the same person and item.
func TestTag(t *testing.T) {
	a := tag("tenant-1", dev, "item-7")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`).MatchString(a) || strings.Contains(a, "0007") {
		t.Errorf("tag = %q", a)
	}
	if a != tag("tenant-1", dev, "item-7") {
		t.Error("the same item gives another tag")
	}
	for _, other := range []string{tag("tenant-2", dev, "item-7"), tag("tenant-1", ben, "item-7"), tag("tenant-1", dev, "item-8"),
		tag("tenant-1", dev, "")} {
		if other == a {
			t.Errorf("another business, person or item gives the same tag %q", a)
		}
	}
}
