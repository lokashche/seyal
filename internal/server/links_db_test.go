//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
)

// Set-your-password links (D62): an admin adds someone or makes a new link; the link works once,
// for a day; a newer link cancels the older; it belongs to one business.
func TestPasswordLinks(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	codeIn := func(page string) string {
		t.Helper()
		m := shownLink.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no link on the page:\n%s", problem(page))
		}
		return m[1]
	}
	welcome := func(code, next, again string) (int, string, http.Header) {
		return s.post(s.browser(), base+"/welcome/"+code, url.Values{"mode": {"choose"}, "next": {next}, "again": {again}}, nil)
	}

	_, added, _ := s.post(l, base+"/settings/people",
		url.Values{"name": {"Vikram Rao"}, "email": {s.mail("vikram")}, "role": {"sales"}}, nil)
	added = html.UnescapeString(added)
	code := codeIn(added)

	t.Run("the admin gets the link and a message to send", func(t *testing.T) {
		for _, want := range []string{"Hi Vikram Rao, here is your Seyal link for Kaveri Associates (demo).",
			"It works once, for 24 hours: http", "Copy message", "not set up yet"} {
			if !strings.Contains(added, want) {
				t.Errorf("People page is missing %q", want)
			}
		}
		if strings.Contains(added, "shown only now") {
			t.Error("a password is still shown on screen")
		}
	})

	t.Run("the link greets the person and isn't cached", func(t *testing.T) {
		status, page, h := s.get(s.browser(), base+"/welcome/"+code)
		if status != http.StatusOK || !strings.Contains(html.UnescapeString(page), "Welcome to Kaveri Associates (demo), Vikram Rao") {
			t.Errorf("welcome page = %d:\n%s", status, page)
		}
		if h.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", h.Get("Cache-Control"))
		}
	})

	t.Run("bad passwords are explained and don't use the link", func(t *testing.T) {
		for _, tt := range []struct{ next, again, want string }{
			{"a-good-new-one", "another-one-12", "The two passwords are different."},
			{"short", "short", "at least 10 characters"},
		} {
			if status, body, _ := welcome(code, tt.next, tt.again); status != http.StatusUnprocessableEntity || !strings.Contains(html.UnescapeString(body), tt.want) {
				t.Errorf("welcome(%q, %q) = %d, want %q", tt.next, tt.again, status, tt.want)
			}
		}
	})

	t.Run("it works once and logs the person in", func(t *testing.T) {
		v := s.useLink(added, "vikram", "vikram-own-pass-1")
		if status, _, _ := s.get(v, base+"/"); status != http.StatusOK {
			t.Errorf("after using the link, My Work = %d", status)
		}
		status, page, _ := welcome(code, "another-pass-12", "another-pass-12")
		if status != http.StatusNotFound || !strings.Contains(page, "expired or was already used") {
			t.Errorf("the same link again = %d", status)
		}
		_, people, _ := s.get(l, base+"/settings/people")
		row := people[strings.Index(people, "<b>Vikram Rao</b>"):]
		if row = row[:strings.Index(row, "</li>")]; strings.Contains(row, "not set up yet") {
			t.Error("Vikram is still shown as not set up")
		}
	})

	t.Run("a newer link cancels the older; the password changes only when it is used", func(t *testing.T) {
		a := s.logIn(arjun)
		_, first, _ := s.post(l, base+"/settings/people/"+s.people["arjun"]+"/link", nil, nil)
		_, second, _ := s.post(l, base+"/settings/people/"+s.people["arjun"]+"/link", nil, nil)
		if status, _, _ := welcome(codeIn(first), "arjun-pass-one-1", "arjun-pass-one-1"); status != http.StatusNotFound {
			t.Errorf("the older link = %d, want it to stop working", status)
		}
		if status, _, _ := s.get(a, base+"/"); status != http.StatusOK {
			t.Error("making a link logged Arjun out; only using it should")
		}
		s.logIn(arjun) // the current password still works
		s.useLink(second, arjun, "arjun-pass-two-2")
		if status, _, _ := s.get(a, base+"/"); status != http.StatusSeeOther {
			t.Error("Arjun's old session survived the new password")
		}
	})

	t.Run("an expired link doesn't work", func(t *testing.T) {
		_, page, _ := s.post(l, base+"/settings/people/"+s.people["arjun"]+"/link", nil, nil)
		// The app may not change when a link expires; the owner ages it.
		err := db.WithTenant(context.Background(), s.owner, s.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `UPDATE login_links SET expires_at = now() - interval '1 minute'
				WHERE tenant_id = current_tenant() AND used_at IS NULL AND cancelled_at IS NULL`)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if status, _, _ := s.get(s.browser(), base+"/welcome/"+codeIn(page)); status != http.StatusNotFound {
			t.Errorf("an expired link = %d, want 404", status)
		}
	})

	t.Run("only admins make links, and not for disabled people", func(t *testing.T) {
		if status, _, _ := s.post(s.logIn(priya), base+"/settings/people/"+s.people["arjun"]+"/link", nil, nil); status != http.StatusForbidden {
			t.Errorf("Priya (not an admin) making a link = %d, want 403", status)
		}
		vikram := s.personID(t, "vikram")
		s.post(l, base+"/settings/people/"+vikram, url.Values{"name": {"Vikram Rao"}, "email": {s.mail("vikram")}, "status": {"disabled"}}, nil)
		status, body, _ := s.post(l, base+"/settings/people/"+vikram+"/link", nil, nil)
		if status != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "disabled") {
			t.Errorf("a link for a disabled person = %d: %s", status, problem(body))
		}
	})

	t.Run("a link belongs to its own business", func(t *testing.T) {
		other := newSite(t)
		_, page, _ := other.post(other.logIn(lakshmi), "/t/"+other.slug+"/settings/people/"+other.people["arjun"]+"/link", nil, nil)
		theirs := codeIn(page)
		if status, body, _ := s.get(s.browser(), base+"/welcome/"+theirs); status != http.StatusNotFound || strings.Contains(body, "Welcome to") {
			t.Errorf("another business's link here = %d", status)
		}
		_, here, _ := s.get(s.browser(), base+"/welcome/"+theirs)
		_, nowhere, _ := s.get(s.browser(), "/t/no-such-firm/welcome/"+theirs)
		if strings.ReplaceAll(nowhere, "no-such-firm", s.slug) != here {
			t.Error("a made-up address and a bad link look different")
		}
	})

	s.tablesMatchLedger(t)
}
