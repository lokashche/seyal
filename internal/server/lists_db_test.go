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

	"seyal/internal/audit"
	"seyal/internal/db"
	"seyal/internal/ledger"
)

// TestSettingsLists: the business's lists are its own, edited without a restart. A change a process
// can't live with is refused, and every change is in the ledger.
func TestSettingsLists(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)

	if code, _, _ := s.get(s.logIn(arjun), base+"/settings/lists"); code != http.StatusForbidden {
		t.Errorf("non-admin opening Lists = %d, want 403", code)
	}
	_, page, _ := s.get(l, base+"/settings/lists")
	for _, want := range []string{">Clients</b>", ">Work types</b>", "4 in use"} {
		if !strings.Contains(page, want) {
			t.Fatalf("lists page is missing %q", want)
		}
	}
	if strings.Contains(page, "Templates you can add") {
		t.Error("templates offered although the business has every sample list")
	}

	// A new list, then an entry in it.
	member := "kind: member\nlabel: Member\nfields:\n  - { key: phone, label: Phone, type: text }\n"
	code, page, _ := s.post(l, base+"/settings/lists", url.Values{"source": {member}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "List added") || !strings.Contains(page, "<b>Phone</b>") {
		t.Fatalf("add list = %d: %s", code, problem(page))
	}
	code, page, _ = s.post(l, base+"/settings/master/member", url.Values{"name": {"Ravi Kumar"}, "f.phone": {"00000 00000"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Ravi Kumar saved.") {
		t.Fatalf("add member = %d: %s", code, problem(page))
	}
	if code, page, _ = s.post(l, base+"/settings/lists", url.Values{"source": {member}}, nil); code != http.StatusUnprocessableEntity ||
		!strings.Contains(problem(page), "already a list") {
		t.Errorf("adding member twice = %d: %s", code, problem(page))
	}

	// Change it: a new detail shows up in its form at once.
	code, page, _ = s.post(l, base+"/settings/lists/member",
		url.Values{"source": {member + "  - { key: joined_on, label: Joined on, type: date }\n"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Saved.") || !strings.Contains(page, "<b>Joined on</b>") {
		t.Fatalf("change list = %d: %s", code, problem(page))
	}
	if _, page, _ = s.get(l, base+"/settings/master/member"); !strings.Contains(page, `name="f.joined_on"`) {
		t.Error("the member form doesn't have the new detail")
	}

	// Refused: removing a detail the process uses, renaming the kind, broken text.
	for _, tt := range []struct{ name, kind, source, want string }{
		{"removing a detail a process uses", "work_type",
			"kind: work_type\nlabel: Work type\nfields:\n  - { key: is_new, label: New, type: bool }\n", "still needs it"},
		{"renaming the kind", "member", strings.Replace(member, "kind: member", "kind: members", 1), "must stay"},
		{"broken text", "member", "kind: member\nlabel: [", "line 2"},
	} {
		code, page, _ := s.post(l, base+"/settings/lists/"+tt.kind, url.Values{"source": {tt.source}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(page), tt.want) {
			t.Errorf("%s = %d: %s; want a problem mentioning %q", tt.name, code, problem(page), tt.want)
		}
		if !strings.Contains(html.UnescapeString(page), tt.source[:12]) {
			t.Errorf("%s: what was typed is not kept in the editor", tt.name)
		}
	}
	if code, _, _ := s.get(l, base+"/settings/lists/nothing"); code != http.StatusNotFound {
		t.Errorf("unknown list = %d, want 404", code)
	}

	err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(context.Background(), `
			SELECT count(*) FROM ledger WHERE kind = 'master_kind.changed' AND body::jsonb->'payload'->>'kind' = 'member'`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("%d ledger events for the member list, want 2 (added, changed)", n)
		}
		rep, err := ledger.Verify(context.Background(), tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(context.Background(), tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
