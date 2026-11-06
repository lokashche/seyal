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
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/setup"
	"seyal/internal/workitem"
)

// TestChasedCards: on the shipped samples (job evaluation 0.6), after Seyal reminds Dev his card says
// so and the thread has the reminder; after Seyal escalates the job to the Owner team, Chitra (an
// admin) sees "Seyal escalated it" with Take it in For my team, and Elan (an Owner, not an admin) sees
// that only people in Operations do this step, so he can only pass it on (D75, M6, decision 9).
func TestChasedCards(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	ctx := context.Background()
	base := "/t/" + s.slug
	for _, key := range []string{"asha", "dev", "chitra", "elan"} {
		var err error
		if s.passwords[key], err = setup.NewPassword(ctx, s.app, s.mail(key), s.tenant); err != nil {
			t.Fatal(err)
		}
	}
	a := s.logIn("asha")
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
	d := s.logIn("dev")
	if code, body, _ := s.post(d, job+"/take", url.Values{"holder": {"team:ops"}, "via": {"button"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Dev's Take = %d: %s", code, problem(body))
	}

	// What the chaser would do on day 2 and day 4 (internal/chase works out when; see its tests).
	zone, _ := time.LoadLocation("Asia/Kolkata")
	chase := func(do func(context.Context, pgx.Tx, string, workitem.Chase) (string, error), days int) {
		t.Helper()
		err := db.WithTenant(ctx, s.app, s.tenant, func(tx pgx.Tx) error {
			it, err := workitem.Get(ctx, tx, id)
			if err != nil {
				return err
			}
			did, err := do(ctx, tx, id, workitem.Chase{StepSince: it.StepSince, Reminders: it.Reminders, Days: days,
				Now: time.Now(), Zone: zone})
			if did == "" && err == nil {
				t.Errorf("nothing was done on day %d", days)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	chase(workitem.Remind, 2)
	_, home, _ = s.get(d, base+"/?f=turn")
	if !strings.Contains(html.UnescapeString(home), "Seyal reminded you · 2 working days in this step") {
		t.Errorf("Dev's card should say Seyal reminded him:\n%s", home)
	}
	today := time.Now().In(zone).Format("Mon 2 Jan")
	if _, page, _ := s.get(d, job); !strings.Contains(html.UnescapeString(page),
		"Reminder for Dev: this has been in With ops for 2 working days (since "+today+"). Still needed: Fee agreed.") {
		t.Errorf("the thread should have the reminder:\n%s", page)
	}

	chase(workitem.Escalate, 4)
	c, e := s.logIn("chitra"), s.logIn("elan")
	_, home, _ = s.get(c, base+"/?f=team")
	home = html.UnescapeString(home)
	if !strings.Contains(home, "Seyal escalated it: waited 4 working days in With ops") ||
		!strings.Contains(home, `/items/`+id+`/take"><input type="hidden" name="holder" value="team:owner">`) {
		t.Errorf("Chitra's For my team should show the escalated job with Take it:\n%s", home)
	}
	_, home, _ = s.get(e, base+"/?f=team")
	home = html.UnescapeString(home)
	if !strings.Contains(home, "Only people in Operations do this step: pass it on") || strings.Contains(home, `/items/`+id+`/take"`) {
		t.Errorf("Elan's For my team should say he can only pass it on, without Take it:\n%s", home)
	}
	if _, page, _ := s.get(e, job); strings.Contains(page, `<input type="hidden" name="action" value="submit">`) ||
		strings.Contains(page, `/items/`+id+`/take"`) || !strings.Contains(page, "Pass on…") {
		t.Errorf("Elan's panel should offer Pass on, not Submit or Take it:\n%s", problem(page))
	}
	if code, _, _ := s.post(e, job+"/take", url.Values{"holder": {"team:owner"}, "via": {"button"}}, nil); code == http.StatusSeeOther {
		t.Error("Elan's Take of the escalated job should be refused, as his card and panel say")
	}
	if _, page, _ := s.get(c, job); !strings.Contains(page, `value="submit"`) || !strings.Contains(html.UnescapeString(page),
		"handed it to the Owner team (escalated): waited 4 working days in With ops (since "+today+"), with Dev since "+today+".") {
		t.Errorf("Chitra (an admin) should see Submit, and the thread the escalation:\n%s", page)
	}

	// Chitra does the Operations work herself and then gives the Owner approval: the approval card says
	// she worked that step after it was escalated (D75, decision 11).
	s.send(c, job+"/field", url.Values{"key": {"fee_agreed"}, "value": {"12000"}})
	s.send(c, job+"/act", url.Values{"action": {"submit"}})
	page := html.UnescapeString(s.send(c, job+"/decide", url.Values{"slot": {"owner"}, "decision": {"approve"}}))
	if !strings.Contains(page, `<span class="r worked">Chitra worked With ops after it was escalated</span>`) ||
		!strings.Contains(page, "Chitra: approved as owner. Worked With ops after it was escalated") {
		t.Errorf("the approval card and the thread should say Chitra worked With ops after it was escalated:\n%s", problem(page))
	}
}
