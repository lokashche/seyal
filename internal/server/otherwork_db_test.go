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

// TestOtherWork: work that fits no process is offered as Other work (the catch-all), which takes its
// "what" from the message instead of asking for it again; naming who does it hands it to them. Work
// that fits a real process is still offered that process.
func TestOtherWork(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	if code, page, _ := s.post(s.logIn(lakshmi), base+"/settings/processes", url.Values{"template": {"other-work"}}, nil); code != http.StatusOK {
		t.Fatalf("add Other work = %d: %s", code, problem(page))
	}
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	if strings.Index(home, `data-name="Other work"`) < strings.Index(home, `data-name="Job evaluation"`) {
		t.Error("the catch-all should be listed after the real processes")
	}
	if strings.Contains(home, `placeholder="For example: Call the bank`) {
		t.Error("the catch-all's example shouldn't be the message box's example")
	}

	tell := func(text string) string {
		_, page, _ := s.post(a, base+"/tell", url.Values{"playbook": {"auto"}, "text": {text}}, nil)
		return html.UnescapeString(page)
	}
	if page := tell("New monthly bookkeeping job for Ganga Traders"); !strings.Contains(page, "This looks like Job evaluation. Start it?") {
		t.Errorf("work that fits a process should be offered that process:\n%s", problem(page))
	}
	page := tell("Water the plants in reception")
	if !strings.Contains(page, "This doesn't fit any of your processes. Start it as Other work?") {
		t.Fatalf("work that fits nothing should be offered Other work:\n%s", problem(page))
	}
	msg := confirmForm.FindStringSubmatch(page)[1]
	pv := regexp.MustCompile(`class="yes" name="playbook" value="([^"]+)"`).FindStringSubmatch(page)[1]
	_, _, h := s.post(a, base+"/tell", url.Values{"message": {msg}, "playbook": {pv}}, nil)
	item := h.Get("Location")
	_, page, _ = s.get(a, item)
	page = html.UnescapeString(page)
	if !strings.Contains(page, "<b>Water the plants in reception</b>") || strings.Contains(page, "What needs doing?") ||
		!strings.Contains(page, "with Arjun Mehta") {
		t.Fatalf("Other work should take its title from the message, ask nothing, and stay with Arjun:\n%s", page)
	}

	// Naming who does it hands it over.
	s.post(a, item+"/field", url.Values{"key": {"who"}, "value": {s.people["priya"]}}, nil)
	if _, page, _ = s.get(a, item); !strings.Contains(html.UnescapeString(page), "with Priya Raman") {
		t.Error("setting who should hand the item to Priya")
	}
	_, home, _ = s.get(s.logIn(priya), base+"/?f=turn")
	if !strings.Contains(home, "Water the plants in reception") {
		t.Error("the item should be in Priya's My turn")
	}
}
