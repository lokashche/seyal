// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

// TestChatLinksSwitch: the Share panel's chat link button is off unless SEYAL_SHARE_CHAT_LINKS says
// "on" (D92), and a typo stops the start instead of guessing.
func TestChatLinksSwitch(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "SEYAL_SHARE_CHAT_LINKS" {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]bool{"": false, "off": false, "on": true} {
		if got, err := chatLinks(env(v)); got != want || err != nil {
			t.Errorf("SEYAL_SHARE_CHAT_LINKS=%q = %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"yes", "ON", "true"} {
		if got, err := chatLinks(env(v)); got || err == nil {
			t.Errorf("SEYAL_SHARE_CHAT_LINKS=%q = %v, %v; want an error and off", v, got, err)
		}
	}
}
