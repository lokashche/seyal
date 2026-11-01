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
// item under "For my team" for everyone in Ops, the item says who has it, and Ops is asked its own
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
	if !strings.Contains(home, "For my team") {
		t.Errorf("Priya (ops) doesn't see the item under For my team")
	}
	_, page, _ = s.get(p, item)
	page = html.UnescapeString(page)
	if !strings.Contains(page, "with Operations team") || !strings.Contains(page, "the Operations team now") ||
		!strings.Contains(page, "What amount?") {
		t.Errorf("after send to ops, the item should be with the Operations team and ask Ops' question:\n%s", problem(page))
	}
	if _, home, _ = s.get(a, base+"/"); strings.Contains(home, "For my team") {
		t.Error("Arjun (sales) sees it under For my team")
	}
}
