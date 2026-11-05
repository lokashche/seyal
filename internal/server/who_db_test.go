//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// holderOnly is a made-up process where only whoever has the item may finish it; the holder, the
// starter, an admin or anyone in Ops may drop it.
const holderOnly = `id: holder-only
version: "1"
description: Ask, hand over to operations, and only operations finish it.
numbering: "W-{seq:4}"
roles: [sales, ops]
fields:
  - { key: who, label: Who, type: text, required: true, ask: "Who is it for?" }
states: [asking, with_ops, done, dropped]
initial: asking
steps:
  asking: { owner: starter, asks: [who], needs: [who] }
  with_ops: { owner: "role:ops" }
transitions:
  - { action: send_to_ops, from: [asking], to: with_ops, when: required_filled, who: [holder] }
  - { action: finish, from: [with_ops], to: done, who: [holder] }
  - { action: drop, from: [asking, with_ops], to: dropped, needs: reason, who: [holder, starter, "role:ops"] }
`

// TestButtonsForThePerson: the side panel shows only the moves the person may take (D68, choice 4).
// Typing a move one may not take gets a plain answer, and nothing moves.
func TestButtonsForThePerson(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	if code, page, _ := s.post(l, base+"/settings/processes",
		url.Values{"action": {"save"}, "fromdraft": {"1"}, "source": {holderOnly}}, nil); code != http.StatusOK {
		t.Fatalf("add process = %d: %s", code, problem(page))
	}
	_, settings, _ := s.get(l, base+"/settings/processes/holder-only")
	if !strings.Contains(html.UnescapeString(settings), "; only whoever has it") {
		t.Errorf("Settings should say who may finish:\n%s", problem(settings))
	}

	a := s.logIn(arjun) // sales, starts it
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Holder only"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer the new process")
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"Something for Ravi"}}, nil)
	item := h.Get("Location")

	// Priya (ops) may not change the details of Arjun's item, but may drop it: the side panel says so.
	_, page, _ := s.get(s.logIn(priya), item)
	if !strings.Contains(page, `value="drop"`) || !strings.Contains(page, "can change the details of this item. You can still use the actions below") {
		t.Errorf("Priya should see Drop, and be told she can use it:\n%s", problem(page))
	}

	s.post(a, item+"/say", url.Values{"text": {"Ravi"}}, nil)
	s.post(a, item+"/say", url.Values{"text": {"send to ops"}}, nil)

	_, page, _ = s.get(a, item)
	if strings.Contains(page, `value="finish"`) || !strings.Contains(page, `value="drop"`) {
		t.Errorf("Arjun (the starter) should see Drop but not Finish:\n%s", problem(page))
	}
	s.post(a, item+"/say", url.Values{"text": {"finish"}}, nil)
	_, page, _ = s.get(a, item)
	if !strings.Contains(html.UnescapeString(page), `only whoever has it (or an admin) can do "finish" here`) {
		t.Errorf("typing finish should get a plain answer:\n%s", problem(page))
	}

	p := s.logIn(priya) // ops: the item is with her team
	_, page, _ = s.get(p, item)
	if !strings.Contains(page, `value="finish"`) {
		t.Errorf("Priya (ops) should see Finish:\n%s", problem(page))
	}
	if !strings.Contains(html.UnescapeString(page), "with Operations team") {
		t.Errorf("finish typed by Arjun moved the item:\n%s", problem(page))
	}
}

// TestSampleButtons follows the shipped Job evaluation 0.5 (the part 1 "Try it"): once Sales sends a
// job to Operations, Sales no longer sees Submit but can still cancel; Operations sees Submit.
func TestSampleButtons(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	a := s.logIn(arjun) // sales
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer Job evaluation:\n%s", problem(home))
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"GST work for Sundaram"}}, nil)
	job := h.Get("Location")
	_, page, _ := s.get(a, job)
	if !strings.Contains(page, `value="send_to_ops"`) {
		t.Errorf("Arjun, who has the draft, should see Send to ops:\n%s", problem(page))
	}
	for _, answer := range []string{"sundaram", "gst", "Monthly GST", "pass", "send to ops"} {
		s.post(a, job+"/say", url.Values{"text": {answer}}, nil)
	}
	_, page, _ = s.get(a, job)
	if !strings.Contains(html.UnescapeString(page), "with Operations team") {
		t.Fatalf("the job should be with the Operations team:\n%s", problem(page))
	}
	if strings.Contains(page, `value="submit"`) || !strings.Contains(page, `value="cancel"`) {
		t.Errorf("Arjun (Sales, the starter) should see Cancel but not Submit:\n%s", problem(page))
	}
	p := s.logIn(priya) // ops
	if _, page, _ = s.get(p, job); !strings.Contains(page, `value="submit"`) || !strings.Contains(page, `value="back_to_sales"`) {
		t.Errorf("Priya (Operations) should see Submit and Back to sales:\n%s", problem(page))
	}
	l := s.logIn(lakshmi) // owner and admin
	if _, page, _ = s.get(l, job); !strings.Contains(page, `value="submit"`) || !strings.Contains(page, `value="cancel"`) {
		t.Errorf("Lakshmi (admin) should see Submit and Cancel:\n%s", problem(page))
	}
}

// TestTemplateSaysWhatItStartsIsBehind: switching Job evaluation to its template leaves the
// Requirement study it starts as it is, and the notice says that one is still on an older version.
func TestTemplateSaysWhatItStartsIsBehind(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	for _, id := range []string{"requirement-study", "job-evaluation"} {
		b, err := os.ReadFile("../../examples/playbooks/" + id + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		older := regexp.MustCompile(`(?m)^version: .*$`).ReplaceAllString(string(b), `version: "0.0-test"`)
		if code, body, _ := s.post(l, base+"/settings/processes/"+id, url.Values{"action": {"save"}, "source": {older}}, nil); code != http.StatusOK {
			t.Fatalf("save an older %s = %d: %s", id, code, problem(body))
		}
	}
	code, page, _ := s.post(l, base+"/settings/processes/job-evaluation", url.Values{"action": {"template"}}, nil)
	page = html.UnescapeString(page)
	if code != http.StatusOK || !strings.Contains(page, "New work now starts on template version") ||
		!strings.Contains(page, "It starts Requirement study, which is still on version 0.0-test") {
		t.Errorf("switch to the template = %d: %s", code, problem(page))
	}
}
