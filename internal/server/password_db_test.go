//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestOwnPassword: a password an admin makes is only for the first login (the person chooses their
// own before anything else); anyone can change their own password, which logs them out elsewhere
// but keeps this device logged in.
func TestOwnPassword(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug

	// Priya has a password an admin made (before links, D62); she must choose her own first.
	temporary := s.temporaryPassword(priya)
	p := s.logIn(priya)
	var page string
	if code, _, h := s.get(p, base+"/"); code != http.StatusSeeOther || h.Get("Location") != base+"/password" {
		t.Fatalf("with a temporary password, My Work = %d to %q; want the password page", code, h.Get("Location"))
	}
	if _, page, _ = s.get(p, base+"/password"); !strings.Contains(page, "only for your first login") {
		t.Errorf("the password page should say why:\n%s", page)
	}

	change := func(c *http.Client, current, next, again string) (int, string) {
		code, body, _ := s.post(c, base+"/password", url.Values{"current": {current}, "next": {next}, "again": {again}}, nil)
		return code, html.UnescapeString(body)
	}
	for _, tt := range []struct{ current, next, again, want string }{
		{"wrong-one-123", "a-good-new-one", "a-good-new-one", "The current password is wrong."},
		{temporary, "a-good-new-one", "another-one-12", "The two new passwords are different."},
		{temporary, "short", "short", "at least 10 characters"},
		{temporary, temporary, temporary, "different from the current one"},
	} {
		if code, body := change(p, tt.current, tt.next, tt.again); code != http.StatusUnprocessableEntity || !strings.Contains(body, tt.want) {
			t.Errorf("change(%q, %q, %q) = %d, want %q", tt.current, tt.next, tt.again, code, tt.want)
		}
	}
	if code, _ := change(p, temporary, "priya-own-pass-1", "priya-own-pass-1"); code != http.StatusSeeOther {
		t.Fatalf("a good change = %d", code)
	}
	if code, _, _ := s.get(p, base+"/"); code != http.StatusOK {
		t.Errorf("after choosing her own, My Work = %d; this device should stay logged in", code)
	}
	s.passwords[priya] = "priya-own-pass-1"
	s.logIn(priya) // the new one works (fails the test if not)

	// Arjun changes his own; his other device is logged out, this one isn't.
	a1, a2 := s.logIn(arjun), s.logIn(arjun)
	if code, _ := change(a1, s.passwords[arjun], "arjun-own-pass-1", "arjun-own-pass-1"); code != http.StatusSeeOther {
		t.Fatalf("Arjun's change = %d", code)
	}
	if code, _, _ := s.get(a1, base+"/"); code != http.StatusOK {
		t.Error("Arjun's device that changed it should stay logged in")
	}
	if code, _, _ := s.get(a2, base+"/"); code != http.StatusSeeOther {
		t.Error("Arjun's other device should be logged out")
	}
}
