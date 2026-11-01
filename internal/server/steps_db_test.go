//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const handOffProcess = `id: hand-off
version: "1"
description: Ask, then hand over to operations.
numbering: "H-{seq:4}"
roles: [sales, ops]
fields:
  - { key: who, label: Who, type: text, required: true, ask: "Who is it for?" }
  - { key: amount, label: Amount, type: money, ask: "What amount?" }
states: [asking, with_ops, done]
initial: asking
steps:
  asking: { owner: starter, asks: [who], needs: [who] }
  with_ops: { owner: "role:ops", asks: [amount], needs: [amount] }
transitions:
  - { action: send_to_ops, from: [asking], to: with_ops, when: required_filled }
  - { action: finish, from: [with_ops], to: done, when: required_filled }
`

// TestStepsOnScreen: the starter is asked only the first step's question; "send to ops" puts the
// item under "For your team" for everyone in Ops, the item says who has it, and Ops is asked its own
// question.
func TestStepsOnScreen(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	if code, page, _ := s.post(l, base+"/settings/processes",
		url.Values{"action": {"save"}, "fromdraft": {"1"}, "source": {handOffProcess}}, nil); code != http.StatusOK {
		t.Fatalf("add process = %d: %s", code, problem(page))
	}

	a := s.logIn(arjun) // sales
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Hand off"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer the new process")
	}
	pv := m[1]
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"Something for Ravi"}}, nil)
	item := h.Get("Location")
	_, page, _ := s.get(a, item)
	if !strings.Contains(page, "Who is it for?") || strings.Contains(page, "What amount?") ||
		!strings.Contains(page, "with Arjun Mehta") {
		t.Fatalf("first step should ask only its own question, with Arjun:\n%s", problem(page))
	}
	s.post(a, item+"/say", url.Values{"text": {"Ravi"}}, nil)
	if _, page, _ = s.get(a, item); !strings.Contains(page, `Say &#34;send to ops&#34;`) &&
		!strings.Contains(html.UnescapeString(page), `Say "send to ops"`) {
		t.Fatalf("after the answer, Seyal should say how to move on:\n%s", problem(page))
	}
	s.post(a, item+"/say", url.Values{"text": {"send to ops"}}, nil)

	p := s.logIn(priya) // ops
	_, home, _ = s.get(p, base+"/")
	if !strings.Contains(home, "For your team") {
		t.Errorf("Priya (ops) doesn't see the item under For your team")
	}
	_, page, _ = s.get(p, item)
	page = html.UnescapeString(page)
	if !strings.Contains(page, "with Operations team") || !strings.Contains(page, "the Operations team now") ||
		!strings.Contains(page, "What amount?") {
		t.Errorf("after send to ops, the item should be with the Operations team and ask Ops' question:\n%s", problem(page))
	}
	if _, home, _ = s.get(a, base+"/"); strings.Contains(home, "For your team") {
		t.Error("Arjun (sales) sees it under For your team")
	}
}

// TestLinkedWorkOnScreen: with the shipped samples, a job for new work shows its study under "Linked
// work", the study shows the job it is part of, and the study is under "For your team" for Ops.
func TestLinkedWorkOnScreen(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	// TDS returns is marked new in the demo data, so it is studied first.
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer Job evaluation:\n%s", problem(home))
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"TDS work for Ganga"}}, nil)
	job := h.Get("Location")
	for _, answer := range []string{"ganga", "tds", "Quarterly returns", "pass", "send to ops"} {
		s.post(a, job+"/say", url.Values{"text": {answer}}, nil)
	}
	_, page, _ := s.get(a, job)
	page = html.UnescapeString(page)
	if !strings.Contains(page, "Linked work") || !strings.Contains(page, "Started</span> <a") ||
		!strings.Contains(page, "Requirement study") || !strings.Contains(page, "study it first") {
		t.Fatalf("the job should show its study:\n%s", problem(page))
	}
	study := regexp.MustCompile(`Started</span> <a href="([^"]+)"`).FindStringSubmatch(page)[1]
	if strings.Count(page, "Started requirement study") != 1 || strings.Contains(page, "added sub-item") || strings.Contains(page, "from this") ||
		strings.Contains(page, "changed Fee proposed by Sales") {
		t.Errorf("the job's thread repeats the link or shows bookkeeping:\n%s", problem(page))
	}
	// Arjun's My Work: the job waits on others and says for what.
	_, home, _ = s.get(a, base+"/")
	waitingGroup := home[strings.Index(home, "Waiting on others"):]
	if !strings.Contains(html.UnescapeString(waitingGroup), "Waiting for requirement study S-") ||
		!strings.Contains(html.UnescapeString(waitingGroup), "(Operations team)") {
		t.Errorf("Arjun's job should wait on others, for the study:\n%s", problem(home))
	}

	p := s.logIn(priya)
	if _, home, _ := s.get(p, base+"/"); !strings.Contains(home, "For your team") || !strings.Contains(home, "requirement study") ||
		!strings.Contains(html.UnescapeString(home), "Seyal is asking: How will we do this work?") {
		t.Errorf("Priya (ops) should see the study under For your team, with Seyal's question:\n%s", problem(home))
	}
	_, page, _ = s.get(p, study)
	if !strings.Contains(page, "Part of</span>") || !strings.Contains(page, "How will we do this work?") {
		t.Errorf("the study should show its job and ask Ops' first question:\n%s", problem(page))
	}
}
