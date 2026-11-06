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

// TestSuggestionOnScreen: a typed hand-over that isn't the whole command becomes a suggestion (D74).
// The page shows Yes and No only to the person who asked, and each says which suggestion it answers.
// Someone else's Yes does nothing; the asker's Yes passes it; the same Yes again does nothing.
func TestSuggestionOnScreen(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	var err error
	if s.passwords["ben"], err = setup.NewPassword(context.Background(), s.app, s.mail("ben"), s.tenant); err != nil {
		t.Fatal(err)
	}
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
	s.send(p, job+"/say", url.Values{"text": {"I'll take it"}})

	page := html.UnescapeString(s.send(p, job+"/say", url.Values{"text": {"give to Ben the March file when it comes"}}))
	button := regexp.MustCompile(`name="button" value="(suggestion:\d+)"><button>Yes, pass it</button>`).FindStringSubmatch(page)
	if !regexp.MustCompile(`Pass J-\d{4}-\d{4} to Ben\?`).MatchString(page) || button == nil {
		t.Fatalf("Priya should be asked to confirm, with Yes and No:\n%s", page)
	}
	if _, other, _ := s.get(b, job); strings.Contains(other, `name="button"`) {
		t.Error("Ben sees the buttons of Priya's suggestion")
	}
	yes := url.Values{"text": {"yes"}, "button": {button[1]}}
	if page = html.UnescapeString(s.send(b, job+"/say", yes)); !strings.Contains(page, "Only Priya Raman can confirm this.") ||
		strings.Contains(page, "passed it to Ben") {
		t.Errorf("Ben's Yes should do nothing:\n%s", problem(page))
	}
	if page = html.UnescapeString(s.send(p, job+"/say", yes)); !strings.Contains(page, "Priya Raman: passed it to Ben") {
		t.Fatalf("Priya's Yes should pass it to Ben:\n%s", problem(page))
	}
	if page = html.UnescapeString(s.send(p, job+"/say", yes)); !strings.Contains(page, "Things changed since; say it again.") {
		t.Errorf("the same Yes again should do nothing:\n%s", problem(page))
	}
	s.tablesMatchLedger(t)
}
