// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"testing"
	"time"

	"seyal/internal/send"
)

// TestRetryAfter: an app's Retry-After, in seconds or as a date, waits at most 6 hours; anything else
// is no wait.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 12, 8, 32, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"120":                           2 * time.Minute,
		"999999999999":                  MaxAsked,
		"Thu, 12 Nov 2026 09:32:00 GMT": time.Hour,
		"Fri, 13 Nov 2026 09:32:00 GMT": MaxAsked,
		"soon":                          0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", v, got, want)
		}
	}
	if len(RetryWaits) != MaxTries-1 {
		t.Errorf("%d waits for %d tries", len(RetryWaits), MaxTries)
	}
}

// TestNotAllowed: a queued body whose field (or link) the app may no longer receive is caught before
// it goes.
func TestNotAllowed(t *testing.T) {
	app := send.Target{Fields: []string{"client", "advance"}}
	for body, want := range map[string]string{
		`{"data":{"fields":{"client":{},"advance":1}}}`:                      "",
		`{"data":{"fields":{"client":{},"fee_agreed":1}}}`:                   "fee_agreed",
		`{"data":{"fields":{"client":{}},"link":"https://seyal.example/x"}}`: "the item's link",
		`not json`: "this body",
	} {
		if got := notAllowed(body, app); got != want {
			t.Errorf("notAllowed(%s) = %q, want %q", body, got, want)
		}
	}
}
