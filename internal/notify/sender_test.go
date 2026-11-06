// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"seyal/internal/send"
)

// TestClassify: what each push service answer means (design 2.1, "The answer").
func TestClassify(t *testing.T) {
	now := time.Date(2026, 11, 6, 16, 40, 0, 0, time.UTC)
	tests := []struct {
		name  string
		a     send.Answer
		err   error
		want  outcome
		wait  time.Duration
		why   string
		reply bool
	}{
		{"201 accepted", send.Answer{Status: 201}, nil, took, 0, "answered 201", true},
		{"200 is fine too", send.Answer{Status: 200}, nil, took, 0, "answered 200", true},
		{"401", send.Answer{Status: 401}, nil, badKey, 0, "answered 401", true},
		{"403", send.Answer{Status: 403}, nil, badKey, 0, "answered 403", true},
		{"404", send.Answer{Status: 404}, nil, gone, 0, "answered 404", true},
		{"410", send.Answer{Status: 410}, nil, gone, 0, "answered 410", true},
		{"429 with seconds", send.Answer{Status: 429, RetryAfter: "120"}, nil, tryAgain, 2 * time.Minute, "answered 429", true},
		{"429 with a date", send.Answer{Status: 429, RetryAfter: now.Add(10 * time.Minute).Format(http.TimeFormat)}, nil, tryAgain,
			10 * time.Minute, "answered 429", true},
		{"429 with a date far ahead", send.Answer{Status: 429, RetryAfter: "Fri, 01 Jan 2100 00:00:00 GMT"}, nil, tryAgain,
			MaxAsked, "answered 429", true},
		{"429 with a huge number", send.Answer{Status: 429, RetryAfter: "99999999999"}, nil, tryAgain, MaxAsked, "answered 429", true},
		{"429 with 2 hours", send.Answer{Status: 429, RetryAfter: "7200"}, nil, tryAgain, MaxAsked, "answered 429", true},
		{"429 with nonsense", send.Answer{Status: 429, RetryAfter: "soon"}, nil, tryAgain, 0, "answered 429", true},
		{"500", send.Answer{Status: 500}, nil, tryAgain, 0, "answered 500", true},
		{"503 with Retry-After", send.Answer{Status: 503, RetryAfter: "60"}, nil, tryAgain, time.Minute, "answered 503", true},
		{"400", send.Answer{Status: 400}, nil, noRetry, 0, "answered 400", true},
		{"413", send.Answer{Status: 413}, nil, noRetry, 0, "answered 413", true},
		{"a redirect", send.Answer{Status: 302}, nil, noRetry, 0, "answered 302", true},
		{"a timeout", send.Answer{}, &send.Failure{Reason: send.ReasonTimeout}, tryAgain, 0, send.ReasonTimeout, false},
		{"no connection", send.Answer{}, &send.Failure{Reason: send.ReasonConnect}, tryAgain, 0, send.ReasonConnect, false},
		{"a private address", send.Answer{}, &send.Failure{Reason: send.ReasonBlocked}, noRetry, 0, send.ReasonBlocked, false},
		{"another error", send.Answer{}, errors.New("Post https://fcm.googleapis.com/fcm/send/secret-0000: boom"), noRetry, 0,
			"could not lock or post it", false},
	}
	for _, tt := range tests {
		got := classify(tt.a, tt.err, now)
		if got.outcome != tt.want || got.wait != tt.wait || got.why != tt.why || got.replied != tt.reply {
			t.Errorf("%s: %+v; want outcome %d, wait %s, why %q, replied %v", tt.name, got, tt.want, tt.wait, tt.why, tt.reply)
		}
		if strings.Contains(got.why, "secret") {
			t.Errorf("%s: the reason holds the address: %q", tt.name, got.why)
		}
	}
}

// TestNextTry: a phone note no device took is tried again after 1, 5 and 30 minutes, then given up;
// a push service that asks for longer gets longer.
func TestNextTry(t *testing.T) {
	now := time.Date(2026, 11, 6, 16, 40, 0, 0, time.UTC)
	for tries, want := range map[int]time.Duration{1: time.Minute, 2: 5 * time.Minute, 3: 30 * time.Minute} {
		if at, ok := nextTry(tries, 0, now); !ok || at.Sub(now) != want {
			t.Errorf("after try %d: %s, %v; want %s", tries, at.Sub(now), ok, want)
		}
	}
	if at, ok := nextTry(1, 2*time.Minute, now); !ok || at.Sub(now) != 2*time.Minute {
		t.Errorf("Retry-After 2 minutes after the first try: %s", at.Sub(now))
	}
	if at, ok := nextTry(2, time.Minute, now); !ok || at.Sub(now) != 5*time.Minute {
		t.Errorf("Retry-After 1 minute after the second try: %s; the usual wait is longer", at.Sub(now))
	}
	for _, tries := range []int{0, MaxTries, MaxTries + 1} {
		if _, ok := nextTry(tries, 0, now); ok {
			t.Errorf("after try %d: tried again", tries)
		}
	}
	// The longest ask (Retry-After is capped at MaxAsked) still comes back well within MaxAge, so the
	// note ends (sent, failed or stale) and never waits forever.
	if at, ok := nextTry(1, retryAfter("Fri, 01 Jan 2100 00:00:00 GMT", now), now); !ok || at.Sub(now) != MaxAsked || MaxAsked >= MaxAge {
		t.Errorf("Retry-After in 2100: tried again after %s", at.Sub(now))
	}
}

// TestPushFamily: each push service checks Seyal's key on its own; the key guard tells them apart by
// the last two parts of the host name.
func TestPushFamily(t *testing.T) {
	for endpoint, want := range map[string]string{
		"https://fcm.googleapis.com/fcm/send/test-0000":                "googleapis.com",
		"https://web.push.apple.com/test-0000":                         "apple.com",
		"https://updates.push.services.mozilla.com/wpush/v2/test-0000": "mozilla.com",
		"https://wns2-par02p.notify.windows.com/w/?token=test-0000":    "windows.com",
		"https://Push.Example:8443/test-0000":                          "push.example",
		"::not a url":                                                  "",
	} {
		if got := pushFamily(endpoint); got != want {
			t.Errorf("pushFamily(%q) = %q; want %q", endpoint, got, want)
		}
	}
}

// TestPayload: the note as the service worker reads it, with the device's own address in front of
// the path, and nothing more than the lock-screen line.
func TestPayload(t *testing.T) {
	n := Note{Body: "New work from Ben: J-2026-0007", Path: "/t/kaveri-demo/items/0000-item", Tag: "AAAA0000aaaa0000BBBB0000bbbb0000"}
	raw, err := n.payload("https://seyal.kaveri-demo.example")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"web_push":8030,"notification":{"title":"Seyal","body":"New work from Ben: J-2026-0007",` +
		`"navigate":"https://seyal.kaveri-demo.example/t/kaveri-demo/items/0000-item","tag":"AAAA0000aaaa0000BBBB0000bbbb0000"}}`
	if string(raw) != want {
		t.Errorf("payload\n got  %s\n want %s", raw, want)
	}
	if !json.Valid(raw) || len(raw) > 500 {
		t.Errorf("payload is %d bytes; the design keeps it under 500", len(raw))
	}
}
