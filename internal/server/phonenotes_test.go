// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"seyal/web"
)

func TestSafeNext(t *testing.T) {
	for _, tt := range []struct {
		next string
		ok   bool
	}{
		{"/t/kaveri-demo/items/0b7e5f4e-1c2d-4a8b-9e3f-000000000007", true},
		{"/t/kaveri-demo/me", true},
		{"/t/kaveri-demo/", false}, // My Work is where a login goes anyway
		{"/t/kaveri-demo", false},
		{"/t/other-firm/items/x", false},
		{"https://evil.example/t/kaveri-demo/x", false},
		{"//evil.example/t/kaveri-demo/x", false},
		{"/t/kaveri-demo//evil.example", false},
		{"/t/kaveri-demo/../other-firm/", false},
		{"/t/kaveri-demo/items/x?y=1", false},
		{`/t/kaveri-demo/items\x`, false},
		{"/t/kaveri-demo/items/%2e%2e", false},
		{"/t/kaveri-demo/items/x#y", false},
		{"/t/kaveri-demo/" + string(make([]byte, 200)), false},
		{"", false},
	} {
		if got := safeNext("kaveri-demo", tt.next); got != tt.ok {
			t.Errorf("safeNext(%q) = %v, want %v", tt.next, got, tt.ok)
		}
	}
	if safeNext("", "/t/kaveri-demo/me") {
		t.Error("a go-back address is kept on / (no business)")
	}
}

func TestDeviceLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36":                       "Chrome on Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1": "Safari on iPhone",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 Edg/130.0":                   "Edge on Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0":                                                        "Firefox on Windows",
		"Mozilla/5.0 (Linux; Android 14; SM-S911B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/26.0 Chrome/122.0 Mobile Safari/537.36":  "Samsung Internet on Android",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15":                      "Safari on a Mac",
		"curl/8.0": "A browser",
	} {
		if got := deviceLabel(ua); got != want {
			t.Errorf("deviceLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestShortDay(t *testing.T) {
	zone := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 11, 6, 16, 40, 0, 0, zone)
	for _, tt := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Hour), "today"},
		{time.Date(2026, 11, 3, 9, 0, 0, 0, zone), "3 Nov"},
		{time.Date(2025, 12, 30, 9, 0, 0, 0, zone), "30 Dec 2025"},
	} {
		if got := shortDay(tt.t, now); got != tt.want {
			t.Errorf("shortDay(%v) = %q, want %q", tt.t, got, tt.want)
		}
	}
}

// TestLoginFormsLoadInFull: a form that logs someone in (login, a welcome link, choosing a password)
// is not boosted, so the page after it loads in full and its <body> carries the new login's
// phone-notes hello. htmx would keep the old <body>, with no hello or the previous login's, and on a
// shared phone the previous person's phone notes would not end. Log-out forms may stay boosted
// (app.js sends them as a full page anyway).
func TestLoginFormsLoadInFull(t *testing.T) {
	form := regexp.MustCompile(`<form method="post"[^>]*>`)
	for _, name := range []string{"login.html", "welcome.html", "password.html"} {
		page, err := fs.ReadFile(web.Files, "templates/"+name)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, tag := range form.FindAllString(string(page), -1) {
			if strings.Contains(tag, "logout") {
				continue
			}
			found++
			if !strings.Contains(tag, `hx-boost="false"`) {
				t.Errorf("%s: %s is boosted, so the next page has no hello for the new login", name, tag)
			}
		}
		if found == 0 {
			t.Errorf("%s: no login form found", name)
		}
	}
}

// TestHelloNeedsTheRoutesAnswer: the page's script counts a phone-note route's reply as sent only
// when it is the route's own JSON. A hello sent on to the password page (a temporary password) comes
// back as that page with status 200; counting it would skip the hello for 6 hours. And a Mac Dock
// app is its own app, like an iPhone icon, so it is marked as this business's.
func TestHelloNeedsTheRoutesAnswer(t *testing.T) {
	js, err := fs.ReadFile(web.Files, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"!r.redirected", `r.headers.get("Content-Type")`, "j.ok = r.ok && json",
		`/^Apple/.test(navigator.vendor`, "if (ownApp()) body.home = true"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js no longer has %q", want)
		}
	}
}
