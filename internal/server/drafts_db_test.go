//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/audit"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// draftAnswer is what a model would answer: a summary, a definition and lists.
func draftAnswer(t *testing.T, summary, definition string, lists ...string) string {
	t.Helper()
	if lists == nil {
		lists = []string{}
	}
	b, err := json.Marshal(map[string]any{"summary": summary, "definition": definition, "lists": lists})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withDrafts switches AI drafting on with a fake that gives the prepared answers in turn.
func (s *site) withDrafts(answers ...string) *ai.Fake {
	n := 0
	f := &ai.Fake{Answer: func(ai.Request) (string, error) {
		if n >= len(answers) {
			return "", ai.ErrFakeDown
		}
		n++
		if answers[n-1] == "down" {
			return "", ai.ErrFakeDown
		}
		return answers[n-1], nil
	}}
	s.ai.Set(ai.Draft, ai.Choice{Name: "fake:test", Model: f})
	return f
}

func (s *site) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	err := db.WithTenant(context.Background(), testdbApp(s.t), s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), query, args...).Scan(&n)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

func sample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDraftAChange: a change described in words becomes a draft, shown with what changes; nothing is
// saved until Save, and then it is the version new work starts on.
func TestDraftAChange(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	proc := base + "/settings/processes/job-evaluation"
	l := s.logIn(lakshmi)

	if _, page, _ := s.get(l, proc); !strings.Contains(page, "AI drafting is switched off") {
		t.Error("with AI off, the page doesn't say drafting is off")
	}

	changed := strings.Replace(sample(t), `version: "0.1"`, `version: "0.2"`, 1)
	changed = strings.Replace(changed, "  - { key: payment_terms,",
		"  - { key: reference, label: Reference number, type: text, required: true, ask: \"What is the reference number?\" }\n  - { key: payment_terms,", 1)
	same := strings.Replace(changed, `version: "0.2"`, `version: "0.1"`, 1) // forgot to change the version
	fake := s.withDrafts(draftAnswer(t, "Kept the version.", same), draftAnswer(t, "Added a required reference number.", changed))

	code, page, _ := s.post(l, proc, url.Values{"action": {"draft"}, "words": {"Also ask for the reference number"}}, nil)
	if code != http.StatusOK {
		t.Fatalf("draft = %d: %s", code, problem(page))
	}
	page = html.UnescapeString(page)
	for _, want := range []string{"Added a required reference number.", "What changes",
		"+ Detail Reference number: text, required, Seyal asks “What is the reference number?”", "Save and use for new work"} {
		if !strings.Contains(page, want) {
			t.Errorf("draft page is missing %q", want)
		}
	}
	seen := fake.Seen()
	if len(seen) != 2 || !strings.Contains(seen[1].Input, "version must change") {
		t.Fatalf("%d calls; the second should carry the first draft's problems", len(seen))
	}
	if n := s.count("SELECT count(*) FROM playbook_versions WHERE playbook_id = 'job-evaluation'"); n != 1 {
		t.Errorf("%d versions after drafting, want 1 (nothing saved yet)", n)
	}
	if n := s.count("SELECT count(*) FROM ai_calls WHERE purpose = 'draft' AND ok"); n != 2 {
		t.Errorf("%d draft calls recorded, want 2", n)
	}
	for _, never := range []string{"Arjun Mehta", "Priya Raman", "kaveri-demo.example", "Kaveri Associates"} {
		if strings.Contains(seen[0].Instructions+seen[0].Input, never) {
			t.Errorf("the request contains %q", never)
		}
	}

	code, page, _ = s.post(l, proc, url.Values{"action": {"save"}, "fromdraft": {"1"}, "source": {changed}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Saved and in use") || !strings.Contains(page, "version 0.2") {
		t.Fatalf("save draft = %d: %s", code, problem(page))
	}
}

// TestDraftThatStaysWrong: after the second bad draft, the problems and the text are shown, and there
// is no Save button. The admin can fix the text and check it again, without AI.
func TestDraftThatStaysWrong(t *testing.T) {
	s := newSite(t)
	proc := "/t/" + s.slug + "/settings/processes/job-evaluation"
	l := s.logIn(lakshmi)
	bad := strings.Replace(sample(t), `version: "0.1"`, `version: "0.3"`, 1)
	bad = strings.Replace(bad, "role: ops", "role: auditor", 1)
	s.withDrafts(draftAnswer(t, "", bad), draftAnswer(t, "", bad))

	code, page, _ := s.post(l, proc, url.Values{"action": {"draft"}, "words": {"Auditors approve instead of ops"}}, nil)
	page = html.UnescapeString(page)
	if code != http.StatusUnprocessableEntity || !strings.Contains(page, "can't be saved yet") ||
		!strings.Contains(page, `unknown role "auditor"`) || strings.Contains(page, "Save and use") ||
		!strings.Contains(page, "role: auditor") {
		t.Fatalf("bad draft = %d:\n%s", code, problem(page))
	}

	fixed := strings.Replace(bad, "role: auditor", "role: ops", 1)
	code, page, _ = s.post(l, proc, url.Values{"action": {"recheck"}, "source": {fixed}, "words": {"x"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Save and use") {
		t.Errorf("recheck after fixing = %d: %s", code, problem(page))
	}
	if n := s.count("SELECT count(*) FROM ai_calls WHERE purpose = 'draft'"); n != 2 {
		t.Errorf("%d draft calls, want 2 (checking again doesn't call AI)", n)
	}
}

// TestDraftANewProcess: a new process with a new list, described in words, is saved together.
func TestDraftANewProcess(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	room := "kind: room\nlabel: Room\nfields:\n  - { key: seats, label: Seats, type: number }\n"
	booking := `id: room-booking
version: "1"
description: Book a meeting room.
numbering: "R-{seq:4}"
title: "{room}"
roles: [ops]
fields:
  - { key: room, label: Room, type: master, kind: room, required: true, ask: "Which room?" }
  - { key: on_date, label: Date, type: date, required: true, ask: "For which day?" }
states: [requested, confirmed, cancelled]
initial: requested
transitions:
  - { action: confirm, from: [requested], to: confirmed, when: required_filled }
  - { action: cancel, from: [requested, confirmed], to: cancelled, needs: reason }
`
	s.withDrafts("down", draftAnswer(t, "A simple room booking.", booking, room))

	code, page, _ := s.post(l, base+"/settings/processes", url.Values{"action": {"draft"}, "words": {"Booking meeting rooms"}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(page, "couldn&#39;t draft this just now") {
		t.Fatalf("draft with the provider down = %d: %s", code, problem(page))
	}
	if n := s.count("SELECT count(*) FROM ai_calls WHERE purpose = 'draft' AND NOT ok"); n != 1 {
		t.Errorf("%d failed draft calls recorded, want 1", n)
	}

	code, page, _ = s.post(l, base+"/settings/processes", url.Values{"action": {"draft"}, "words": {"Booking meeting rooms"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "New list: Rooms") || !strings.Contains(page, "Draft of a new process") {
		t.Fatalf("draft new = %d: %s", code, problem(page))
	}
	code, page, _ = s.post(l, base+"/settings/processes",
		url.Values{"action": {"save"}, "fromdraft": {"1"}, "source": {booking}, "list": {room}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Saved and in use") {
		t.Fatalf("save new = %d: %s", code, problem(page))
	}
	// Saving the same new process again is refused: it exists now.
	code, page, _ = s.post(l, base+"/settings/processes",
		url.Values{"action": {"save"}, "fromdraft": {"1"}, "source": {booking}, "list": {room}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(html.UnescapeString(page), "already a process") {
		t.Errorf("saving it twice = %d: %s", code, problem(page))
	}

	err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		ctx := context.Background()
		k, err := playbook.Kind(ctx, tx, "room")
		if err != nil || k == nil {
			t.Errorf("room list = %v, %v", k, err)
		}
		inUse, err := playbook.ListInUse(ctx, tx)
		if err != nil {
			return err
		}
		if len(inUse) != 2 {
			t.Errorf("%d processes in use, want 2", len(inUse))
		}
		rep, err := ledger.Verify(ctx, tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(ctx, tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
