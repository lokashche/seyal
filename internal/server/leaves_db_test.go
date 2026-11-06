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

// TestLeavesSeyal: the "Leaves Seyal" part of Settings → Processes (D89). An admin pauses and resumes
// phone notes for the business; each press is a ledger event by the admin. Me says when this business
// sends none. Nothing is sent anywhere.
func TestLeavesSeyal(t *testing.T) {
	plain := newDemoSite(t)
	s, _ := plain.withPush(t)
	admin := s.logIn(lakshmi)
	page := func(c *http.Client, path string) string {
		t.Helper()
		code, body, _ := s.get(c, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		return html.UnescapeString(body)
	}
	has := func(what, body string, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if !strings.Contains(body, p) {
				t.Errorf("%s: no %q on the page", what, p)
			}
		}
	}
	processes := "/t/" + s.slug + "/settings/processes"

	has("on", page(admin, processes), "Leaves Seyal", "Phone notes", ">on<", "Working hours 09:00-19:00", `value="pause"`,
		`aria-label="Pause phone notes for this business"`)

	code, body, _ := s.post(admin, "/t/"+s.slug+"/settings/phone-notes", url.Values{"do": {"pause"}}, nil)
	body = html.UnescapeString(body)
	if code != http.StatusOK {
		t.Fatalf("Pause = %d: %s", code, problem(body))
	}
	has("paused", body, "are paused", "Paused by Lakshmi Narayanan at ", `value="resume"`,
		`aria-label="Resume phone notes for this business"`)
	has("Me while paused", page(s.logIn(priya), "/t/"+s.slug+"/me"), "are paused by an admin")

	if code, _, _ := s.post(s.logIn(arjun), "/t/"+s.slug+"/settings/phone-notes", url.Values{"do": {"resume"}}, nil); code != http.StatusForbidden {
		t.Errorf("Resume by someone who isn't an admin = %d, want 403", code)
	}
	if code, _, _ := s.post(admin, "/t/"+s.slug+"/settings/phone-notes", url.Values{"do": {"resume"}}, nil); code != http.StatusOK {
		t.Errorf("Resume = %d", code)
	}
	has("resumed", page(admin, processes), ">on<", `value="pause"`)

	err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(context.Background(), `SELECT count(*) FROM ledger WHERE kind = 'business.updated'
			AND actor_person_id::text = $1 AND body::jsonb #> '{payload,changes}' ? 'phone_notes_paused'`, s.people[lakshmi]).Scan(&n)
		if n != 2 {
			t.Errorf("%d pause events by Lakshmi, want 2 (Pause, Resume; not Arjun's refused press)", n)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// The set-up file has them off: Settings and Me say so.
	if _, err := s.owner.Exec(context.Background(), "UPDATE tenants SET notifications = false WHERE id = $1", s.tenant); err != nil {
		t.Fatal(err)
	}
	has("file off", page(admin, processes), "doesn't turn them on, so nobody here gets a phone note",
		"add notifications: on to the set-up file and import it again")
	has("Me with the file off", page(admin, "/t/"+s.slug+"/me"), "its set-up file doesn't turn them on")

	// A server without SEYAL_PUSH: nothing leaves Seyal.
	has("server off", page(plain.logIn(lakshmi), processes), "off on this server")
}
