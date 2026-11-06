//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/setup"
	"seyal/internal/testdb"
)

// TestTypedHandOversOnScreen: hand-over words typed in the Work Item's chat (D73). "I'll take it" takes
// the job; "pass to ben, …" when two people are called Ben asks which one, with a button each that only
// the person who asked sees; the button sends the whole command, and the thread says who passed it.
func TestTypedHandOversOnScreen(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	ctx := context.Background()
	var err error
	if s.passwords["ben"], err = setup.NewPassword(ctx, s.app, s.mail("ben"), s.tenant); err != nil {
		t.Fatal(err)
	}
	// A second Ben, in Operations, with a login.
	var benKumar string
	if err := db.WithTenant(ctx, s.owner, s.tenant, func(tx pgx.Tx) error {
		var err error
		if benKumar, err = people.Add(ctx, tx, "Ben Kumar", testdb.Email("benk"), ledger.SystemActor); err != nil {
			return err
		}
		_, err = people.Grant(ctx, tx, benKumar, "ops", ledger.SystemActor)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	testdb.GiveLogins(t, s.owner, s.tenant, benKumar)

	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer Job evaluation:\n%s", problem(home))
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"Monthly books for Sundaram"}}, nil)
	job := h.Get("Location")
	for _, answer := range []string{"sundaram", "monthly bookkeeping", "Books for the year", "pass", "send to ops"} {
		s.post(a, job+"/say", url.Values{"text": {answer}}, nil)
	}

	p, b := s.logIn(priya), s.logIn("ben")
	page := html.UnescapeString(s.send(p, job+"/say", url.Values{"text": {"I'll take it"}}))
	if !strings.Contains(page, "Priya Raman: took it from the Operations team") || !strings.Contains(page, "It's yours now.") {
		t.Fatalf("I'll take it should take the job:\n%s", problem(page))
	}

	page = html.UnescapeString(s.send(p, job+"/say", url.Values{"text": {"pass to ben, he is free"}}))
	button := `<input type="hidden" name="text" value="pass to Ben Kumar, he is free"><button>Ben Kumar</button>`
	if !strings.Contains(page, "Which one do you mean: Ben, or Ben Kumar?") || !strings.Contains(page, button) {
		t.Fatalf("Priya should be asked which Ben, with a button each:\n%s", page)
	}
	if _, other, _ := s.get(b, job); strings.Contains(html.UnescapeString(other), button) {
		t.Error("Ben sees the buttons of Priya's question")
	}
	page = html.UnescapeString(s.send(p, job+"/say", url.Values{"text": {"pass to Ben Kumar, he is free"}}))
	if !strings.Contains(page, "Priya Raman: passed it to Ben Kumar: he is free") || strings.Contains(page, button) {
		t.Errorf("the button's command should pass it to Ben Kumar, and the buttons go:\n%s", page)
	}
}
