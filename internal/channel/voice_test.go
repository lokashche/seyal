// SPDX-License-Identifier: AGPL-3.0-or-later

package channel

import "testing"

func TestEdited(t *testing.T) {
	raw := "Acme Traders wants monthly books"
	for _, c := range []struct {
		text string
		want bool
	}{
		{raw, false},
		{"For the new year: " + raw, false},  // typed before recording, words untouched
		{"For the new year:\n" + raw, false}, // on its own line
		{"Acme Traders wants yearly books", true},
		{raw + " from April", true}, // words added after the heard ones
		{"Acme" + raw, true},        // no space: the heard words were changed
		{"Acme Traders\r\nwants monthly books", true},
	} {
		if got := edited(c.text, raw); got != c.want {
			t.Errorf("edited(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	if edited("Line one\r\nline two", "Line one\nline two") {
		t.Error("a line break sent as CRLF counts as a correction")
	}
}
