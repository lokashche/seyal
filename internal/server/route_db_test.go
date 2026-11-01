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

	"seyal/internal/ai"
)

// twoProcesses gives the demo business a second process (the internal request template), so Tell
// Seyal has something to decide between.
func (s *site) twoProcesses() {
	s.t.Helper()
	l := s.logIn(lakshmi)
	if code, page, _ := s.post(l, "/t/"+s.slug+"/settings/processes", url.Values{"template": {"internal-request"}}, nil); code != http.StatusOK {
		s.t.Fatalf("add the internal request process = %d: %s", code, problem(page))
	}
}

var confirmForm = regexp.MustCompile(`name="message" value="([^"]+)"`)

// TestLetSeyalDecide: with "let Seyal decide", the message is saved and Seyal asks to confirm the
// process it suggests; nothing starts until the person taps a button. They can pick another one.
func TestLetSeyalDecide(t *testing.T) {
	s := newSite(t)
	s.twoProcesses()
	base := "/t/" + s.slug
	a := s.logIn(arjun)

	_, home, _ := s.get(a, base+"/")
	for _, want := range []string{`value="auto" form="tell-form"`, "Let Seyal decide", "data-proc-search", ">Internal request</b>"} {
		if !strings.Contains(home, want) {
			t.Fatalf("Tell Seyal is missing %q", want)
		}
	}
	jobPV := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)[1]
	reqPV := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Internal request"`).FindStringSubmatch(home)[1]

	// Without AI, the words decide: "printer" and "office" are in the request's examples.
	code, page, _ := s.post(a, base+"/tell", url.Values{"playbook": {"auto"}, "text": {"The printer in my office is broken again"}}, nil)
	page = html.UnescapeString(page)
	if code != http.StatusOK || !strings.Contains(page, "This looks like Internal request. Start it?") ||
		!strings.Contains(page, `value="`+reqPV+`">Yes, start Internal request`) || !strings.Contains(page, ">Job evaluation</button>") {
		t.Fatalf("auto = %d, want a suggestion to confirm:\n%s", code, problem(page))
	}
	if n := s.count("SELECT count(*) FROM work_items"); n != 0 {
		t.Errorf("%d work items before confirming, want none", n)
	}
	msg := confirmForm.FindStringSubmatch(page)[1]

	// Someone else can't start Arjun's message.
	p := s.logIn(priya)
	if code, _, _ := s.post(p, base+"/tell", url.Values{"message": {msg}, "playbook": {reqPV}}, nil); code == http.StatusSeeOther {
		t.Error("Priya started Arjun's message")
	}

	// Arjun picks the other process instead; the message becomes the first entry of that item.
	code, _, h := s.post(a, base+"/tell", url.Values{"message": {msg}, "playbook": {jobPV}}, nil)
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/items/") {
		t.Fatalf("confirm = %d", code)
	}
	_, page, _ = s.get(a, h.Get("Location"))
	if !strings.Contains(page, "The printer in my office is broken again") || !strings.Contains(page, "Which client is this for?") {
		t.Errorf("the item should start with the message and ask its first question:\n%s", problem(page))
	}
	if code, _, _ := s.post(a, base+"/tell", url.Values{"message": {msg}, "playbook": {reqPV}}, nil); code == http.StatusSeeOther {
		t.Error("the same message started a second item")
	}

	// Nothing matches: Seyal asks which process, with no suggestion.
	_, page, _ = s.post(a, base+"/tell", url.Values{"playbook": {"auto"}, "text": {"Zzz qqq"}}, nil)
	if !strings.Contains(page, "Which process is this for?") || strings.Contains(page, "Yes, start") {
		t.Errorf("no match should ask which process:\n%s", problem(page))
	}

	// A greeting starts nothing and asks nothing.
	_, page, _ = s.post(a, base+"/tell", url.Values{"playbook": {"auto"}, "text": {"Good morning"}}, nil)
	if !strings.Contains(page, "Tell me about the work you want to start") || strings.Contains(page, "Start it?") {
		t.Errorf("a greeting should get a reply:\n%s", problem(page))
	}

	// Picking a process directly starts it at once.
	if code, _, _ := s.post(a, base+"/tell", url.Values{"playbook": {reqPV}, "text": {"Need a new chair"}}, nil); code != http.StatusSeeOther {
		t.Errorf("a chosen process = %d, want it to start at once", code)
	}
}

// TestLetSeyalDecideWithAI: with AI on, the model's choice is suggested (still confirmed by the person).
func TestLetSeyalDecideWithAI(t *testing.T) {
	s := newSite(t)
	s.twoProcesses()
	fake := &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if strings.Contains(r.Instructions, "decide which process") {
			return `{"kind":"work","process":"job-evaluation","reply":""}`, nil
		}
		return `{"kind":"note","fields":{},"new_items":[],"command":"","reason":"","reply":""}`, nil
	}}
	s.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: fake})
	a := s.logIn(arjun)
	_, page, _ := s.post(a, "/t/"+s.slug+"/tell", url.Values{"playbook": {"auto"}, "text": {"Sundaram needs GST filing every month"}}, nil)
	if !strings.Contains(html.UnescapeString(page), "This looks like Job evaluation. Start it?") {
		t.Fatalf("the AI's choice wasn't suggested:\n%s", problem(page))
	}
	seen := fake.Seen()
	if len(seen) != 1 || !strings.Contains(seen[0].Instructions, "id internal-request") || strings.Contains(seen[0].Instructions, "Arjun") {
		t.Errorf("routing request = %+v", seen)
	}
}

// TestMyWorkFolders: My Work shows folders with counts and a filter per process; the list shows the
// chosen folder, and links keep the choice.
func TestMyWorkFolders(t *testing.T) {
	s := newSite(t)
	s.twoProcesses()
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	jobPV := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)[1]
	reqPV := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Internal request"`).FindStringSubmatch(home)[1]
	for _, m := range []struct{ pv, text string }{{jobPV, "Work for Sundaram"}, {jobPV, "Work for Ganga"}, {reqPV, "Need a new chair"}} {
		if code, _, _ := s.post(a, base+"/tell", url.Values{"playbook": {m.pv}, "text": {m.text}}, nil); code != http.StatusSeeOther {
			t.Fatalf("start %q = %d", m.text, code)
		}
	}

	_, home, _ = s.get(a, base+"/")
	folder := regexp.MustCompile(`<a class="fold" href="[^"]*f=turn[^"]*" aria-current="true"><svg[^>]*>.*?</svg><span class="ft">My turn</span><span class="n">3</span>`)
	if !folder.MatchString(home) {
		t.Fatalf("My turn should be the open folder, with 3:\n%s", problem(home))
	}
	for _, want := range []string{"<span class=\"ft\">All open work</span><span class=\"n\">3</span>",
		"<span class=\"ft\">Job evaluation</span><span class=\"n\">2</span>", "<span class=\"ft\">Internal request</span><span class=\"n\">1</span>",
		"Seyal is asking: Which client is this for?", "“Work for Ganga”", `class="avatar sm" title="With Arjun Mehta">AM</span>`} {
		if !strings.Contains(html.UnescapeString(home), want) {
			t.Errorf("My Work is missing %q", want)
		}
	}

	// A process shows all its open work, whatever folder was open, so its count matches the list; and
	// links keep the choice.
	for _, q := range []string{"/?p=internal-request", "/?f=turn&p=internal-request"} {
		_, list, _ := s.get(a, base+q)
		if strings.Contains(list, "Work for Sundaram") || !strings.Contains(list, "Need a new chair") ||
			!strings.Contains(list, `<b>Internal request</b><span class="count">1</span>`) ||
			!strings.Contains(list, "?f=all&amp;p=internal-request") {
			t.Errorf("%s should show only the request, and keep itself on links:\n%s", q, problem(list))
		}
	}
	if _, list, _ := s.get(a, base+"/?f=nonsense&s=due"); !strings.Contains(list, `aria-current="true">Due</a>`) {
		t.Errorf("sorting by due date should be marked:\n%s", problem(list))
	}
}
