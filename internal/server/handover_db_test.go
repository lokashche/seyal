//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/setup"
)

// TestTakeAndPassOnScreen: with the shipped samples, a job sent to Operations shows "Take it" on the
// team's cards and in the side panel; the first taker gets it and the second is told who did; the
// holder sees Pass on (only people in Operations, and the team) and Give back to the team; a note
// shows on the receiver's card; an admin may pass outside the list with a warning; nobody from another
// business or another site gets in; and an admin sees work nobody can take (D68, M1, M2, choice 13).
func TestTakeAndPassOnScreen(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	var err error
	if s.passwords["ben"], err = setup.NewPassword(context.Background(), s.app, s.mail("ben"), s.tenant); err != nil {
		t.Fatal(err)
	}
	ben, priyaID, arjunID := s.people["ben"], s.people[priya], s.people[arjun]

	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer Job evaluation:\n%s", problem(home))
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"Monthly books for Sundaram"}}, nil)
	job := h.Get("Location")
	id := job[strings.LastIndex(job, "/")+1:]
	for _, answer := range []string{"sundaram", "monthly bookkeeping", "Books for the year", "pass", "send to ops"} {
		s.post(a, job+"/say", url.Values{"text": {answer}}, nil)
	}

	// Both Ops people see Take it on the card under For my team, and in the panel.
	b, p := s.logIn("ben"), s.logIn(priya)
	takeForm := `action="` + base + `/items/` + id + `/take"><input type="hidden" name="holder" value="team:ops"><input type="hidden" name="via" value="button">`
	for who, c := range map[string]*http.Client{"Ben": b, "Priya": p} {
		_, home, _ := s.get(c, base+"/?f=team")
		if !strings.Contains(home, takeForm) {
			t.Fatalf("%s should see Take it on the team's card:\n%s", who, home)
		}
		// The card has its own class (".card" is the login box), and the button's name starts with its words.
		if !strings.Contains(home, `<div class="work-card">`) || strings.Contains(home, `<div class="card">`) ||
			!strings.Contains(home, `aria-label="Take it: `) {
			t.Errorf("%s's card should be a work-card whose button is named \"Take it: ...\":\n%s", who, home)
		}
	}
	if _, home, _ := s.get(a, base+"/?f=all"); strings.Contains(home, "/take") {
		t.Error("Arjun (sales) shouldn't be offered Take it")
	}
	if _, page, _ := s.get(p, job); !strings.Contains(page, `/take"><input type="hidden" name="holder" value="team:ops"><button`) {
		t.Errorf("Priya should see Take it in the side panel:\n%s", problem(page))
	}

	// Cross-site posts are refused before anything is read (the browser's own forms only).
	if code, _, _ := s.post(b, job+"/take", url.Values{"holder": {"team:ops"}}, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("a cross-site Take = %d; want 403", code)
	}
	// Someone from another business can't reach the item, even with its id.
	other := newSiteWith(t, "../../examples/playbooks")
	if code, _, _ := other.post(other.logIn(priya), "/t/"+other.slug+"/items/"+id+"/take", url.Values{"holder": {"team:ops"}}, nil); code != http.StatusNotFound {
		t.Errorf("Take from another business = %d; want 404", code)
	}

	// Ben takes it from his card; Priya's page still showed it with the team.
	if code, body, _ := s.post(b, job+"/take", url.Values{"holder": {"team:ops"}, "via": {"button"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Ben's Take = %d: %s", code, problem(body))
	}
	code, body, _ := s.post(p, job+"/take", url.Values{"holder": {"team:ops"}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "Ben took it a moment ago, so nothing was changed. Ask Ben to pass it to you") {
		t.Errorf("Priya's late Take = %d: %s", code, problem(body))
	}
	if _, home, _ := s.get(p, base+"/?f=team"); strings.Contains(home, "Monthly books") || strings.Contains(home, "Sundaram") {
		t.Error("the job should have left Priya's For my team")
	}

	// Ben's panel: since today, in this step since today (D76), Pass on with only Ops people and the
	// team, the privacy line, Give back.
	_, page, _ := s.get(b, job)
	page = html.UnescapeString(page)
	for _, want := range []string{"with Ben · since today · in this step since today", "Pass on…", `value="person:` + priyaID + `">Priya Raman`,
		`value="team:ops">the Operations team`, "Everyone in the business can see this, and it can't be deleted.",
		"Give back to the Operations team", `name="holder" value="person:` + ben + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("Ben's panel should contain %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `value="person:`+arjunID+`"`) || strings.Contains(page, "Others (as an admin)") {
		t.Error("Ben's Pass on list offers someone outside Operations")
	}

	// Passing to Arjun is refused on the server too, and the form comes back open with the choice and
	// the note kept; passing to Priya with a note works.
	code, body, _ = s.post(b, job+"/pass", url.Values{"to": {"person:" + arjunID}, "note": {"for the year end"},
		"holder": {"person:" + ben}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "Arjun Mehta can't receive it in this step. It can go to: Priya Raman, or the Operations team") {
		t.Errorf("passing to Arjun = %d: %s", code, problem(body))
	}
	if !strings.Contains(body, `<details class="pass" open>`) || !strings.Contains(body, `value="for the year end"`) {
		t.Errorf("a refused Pass on should come back open with the note:\n%s", body)
	}
	code, body, _ = s.post(b, job+"/pass", url.Values{"to": {"team:ops"}, "note": {"x"}, "holder": {"team:sales"}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `<option value="team:ops" selected>`) {
		t.Errorf("a refused Pass on should keep the choice = %d:\n%s", code, problem(body))
	}
	page = html.UnescapeString(s.send(b, job+"/pass", url.Values{"to": {"person:" + priyaID}, "note": {"she knows Sundaram well"},
		"holder": {"person:" + ben}}))
	if !strings.Contains(page, "Ben: passed it to Priya Raman: she knows Sundaram well") {
		t.Errorf("the thread should say Ben passed it, with the note:\n%s", page)
	}
	if _, home, _ := s.get(p, base+"/?f=turn"); !strings.Contains(html.UnescapeString(home), "From Ben: she knows Sundaram well") {
		t.Errorf("Priya's card should show the note:\n%s", home)
	}
	// Ben's old page now does nothing.
	code, body, _ = s.post(b, job+"/pass", url.Values{"to": {"team:ops"}, "holder": {"person:" + ben}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "It went to Priya Raman a moment ago") {
		t.Errorf("a pass from Ben's old page = %d: %s", code, problem(body))
	}

	// Lakshmi (admin, not in Operations) sees everyone, and passing outside the list warns.
	l := s.logIn(lakshmi)
	_, page, _ = s.get(l, job)
	if !strings.Contains(page, "Others (as an admin)") || !strings.Contains(page, `value="person:`+arjunID+`"`) {
		t.Errorf("an admin should be offered everyone:\n%s", problem(page))
	}
	code, body, _ = s.post(l, job+"/pass", url.Values{"to": {"person:" + arjunID}, "holder": {"person:" + priyaID}}, nil)
	if code != http.StatusOK || !strings.Contains(html.UnescapeString(body), "Passed to Arjun Mehta as an admin. In this step, work normally goes to people in Operations. "+
		"Arjun Mehta isn't in Operations, so they can only pass it on here.") {
		t.Errorf("an admin's pass outside the list = %d:\n%s", code, problem(body))
	}
	if strings.Contains(body, `<details class="pass" open>`) {
		t.Error("after a pass that worked, the Pass on form should be closed")
	}
	// Arjun has it but isn't in Operations: his card and panel tell him to pass it on, not to do the step.
	if _, home, _ := s.get(a, base+"/?f=turn"); !strings.Contains(html.UnescapeString(home), "Only people in Operations do this step: pass it on") ||
		strings.Contains(html.UnescapeString(home), "Seyal is asking") {
		t.Errorf("Arjun's card should tell him to pass it on:\n%s", home)
	}
	if _, page, _ := s.get(a, job); !strings.Contains(page, "You have this, but only people in Operations can do this step, so pass it on") {
		t.Errorf("Arjun's panel should tell him to pass it on:\n%s", problem(page))
	}

	// Arjun, who has it now, gives it to the Operations team; then nobody is left in Operations who
	// can take it, and Lakshmi (admin) sees it, marked.
	if _, body, _ = s.post(a, job+"/pass", url.Values{"to": {"team:ops"}, "holder": {"person:" + arjunID}}, nil); problem(body) != "(no problem shown)" {
		t.Fatalf("Arjun giving it to the Operations team: %s", problem(body))
	}
	if err := db.WithTenant(context.Background(), s.owner, s.tenant, func(tx pgx.Tx) error {
		for _, person := range []string{ben, priyaID} {
			if _, err := people.SetActive(context.Background(), tx, person, false, ledger.SystemActor); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, home, _ = s.get(l, base+"/?f=team")
	if !strings.Contains(home, "Nobody can take this") || !strings.Contains(home, "Sundaram") {
		t.Errorf("Lakshmi should see the job nobody can take under For my team:\n%s", home)
	}
	if _, page, _ = s.get(l, job); !strings.Contains(page, "Nobody can take this. As an admin, pass it on") {
		t.Errorf("the panel should tell an admin nobody can take it:\n%s", problem(page))
	}
}
