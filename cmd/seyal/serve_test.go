// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

// TestSecureCookies: HTTPS-only cookies unless switched off or listening only on this computer.
func TestSecureCookies(t *testing.T) {
	for _, tt := range []struct {
		setting, addr string
		want          bool
	}{
		{"", "", false},                  // default address: this computer only
		{"", "127.0.0.1:8080", false},    // development
		{"", "localhost:8080", false},    // development
		{"", ":8080", true},              // in a container or on a server: secure
		{"", "0.0.0.0:8080", true},       // listening for others: secure
		{"false", ":8080", false},        // switched off on purpose
		{"true", "127.0.0.1:8080", true}, // switched on on purpose
	} {
		if got := secureCookies(tt.setting, tt.addr); got != tt.want {
			t.Errorf("secureCookies(%q, %q) = %v, want %v", tt.setting, tt.addr, got, tt.want)
		}
	}
}
