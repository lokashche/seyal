//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"seyal/internal/ai"
	"seyal/internal/server"
)

var itemID = regexp.MustCompile(`/items/([0-9a-f-]{36})$`)

// TestAskOnTellSeyal: a question on Tell Seyal, typed or spoken, gets an answer from the data with
// links to the items, and starts nothing. One login in two businesses only ever sees the items of the
// business it asks in.
func TestAskOnTellSeyal(t *testing.T) {
	a, b := newSite(t), newSite(t)
	a.join(b, arjun, "sales")
	understand := &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if strings.Contains(r.Instructions, "small search") {
			return `{"kind":"query","processes":[],"status":"any","person":"me","role":"started","entry":"","period":"this_week","count":false}`, nil
		}
		kind := "new_work"
		if strings.Contains(r.Input, "What did I start") {
			kind = "question"
		}
		return `{"kind":"` + kind + `","fields":{},"new_items":[],"command":"","reason":"","reply":""}`, nil
	}}
	a.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: understand})
	speech := &ai.FakeSpeech{Answer: func(ai.Audio) (ai.Transcript, error) {
		return ai.Transcript{Text: "What did I start this week?", Seconds: 2}, nil
	}}
	a.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: speech})

	c := a.logIn(arjun) // the same login reaches Arjun's person in b
	inA, inB := "/t/"+a.slug, "/t/"+b.slug
	pv := func(base string) string {
		_, home, _ := a.get(c, base+"/")
		m := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
		if m == nil {
			t.Fatalf("no process on %s:\n%s", base, problem(home))
		}
		return m[1]
	}
	start := func(base, text string) string {
		code, body, h := a.post(c, base+"/tell", url.Values{"playbook": {pv(base)}, "text": {text}}, nil)
		m := itemID.FindStringSubmatch(h.Get("Location"))
		if code != http.StatusSeeOther || m == nil {
			t.Fatalf("start %q in %s = %d: %s", text, base, code, problem(body))
		}
		return m[1]
	}
	itemA := start(inA, "Work for Sundaram Textiles")
	itemB := start(inB, "Work for Ganga Traders")

	items := a.count("SELECT count(*) FROM work_items")
	for _, tt := range []struct{ base, mine, theirs string }{{inA, itemA, itemB}, {inB, itemB, itemA}} {
		code, page, h := a.post(c, tt.base+"/tell", url.Values{"playbook": {pv(tt.base)}, "text": {"What did I start this week?"}}, nil)
		page = html.UnescapeString(page)
		if code != http.StatusOK || h.Get("Location") != "" {
			t.Fatalf("asking in %s = %d to %q", tt.base, code, h.Get("Location"))
		}
		if !strings.Contains(page, "1 item started by you this week (") || !strings.Contains(page, `href="`+tt.base+`/items/`+tt.mine+`"`) {
			t.Errorf("asking in %s should list its own item:\n%s", tt.base, page)
		}
		if strings.Contains(page, tt.theirs) {
			t.Errorf("asking in %s shows the other business's item", tt.base)
		}
		if !strings.Contains(page, "<p>What did I start this week?</p>") {
			t.Errorf("the question isn't shown above the answer")
		}
	}

	// Spoken: the words come back to correct, then go out like typed words.
	recording := ai.FakeRecording(2)
	_, ans := a.speak(c, "", recording, "audio/webm")
	if ans.Text != "What did I start this week?" || ans.Token == "" {
		t.Fatalf("voice = %+v", ans)
	}
	code, page, h := a.sendSpoken(c, inA+"/tell", ans.Text, ans.Token, recording, "playbook", pv(inA))
	if code != http.StatusOK || h.Get("Location") != "" || !strings.Contains(page, `href="`+inA+`/items/`+itemA+`"`) {
		t.Errorf("a spoken question = %d to %q:\n%s", code, h.Get("Location"), problem(page))
	}
	if n := a.count("SELECT count(*) FROM work_items"); n != items {
		t.Errorf("asking started %d items", n-items)
	}
	a.tablesMatchLedger(t)
}

// TestAskLimitsOnTellSeyal: with AI off, Tell Seyal doesn't suggest asking, and a clear question gets
// the plain "needs AI" with a button that starts the same message as work. With AI on, the hint shows,
// and a login that asks too many questions in a minute is told to wait without the model being asked.
func TestAskLimitsOnTellSeyal(t *testing.T) {
	a := newSite(t)
	c := a.logIn(arjun)
	base := "/t/" + a.slug
	_, home, _ := a.get(c, base+"/")
	if strings.Contains(home, "You can also ask") {
		t.Error("with AI off, Tell Seyal suggests asking about the work")
	}
	m := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("no process:\n%s", problem(home))
	}
	pv := m[1]
	code, page, _ := a.post(c, base+"/tell", url.Values{"playbook": {pv}, "text": {"What is overdue?"}}, nil)
	page = html.UnescapeString(page)
	msg := regexp.MustCompile(`name="message" value="([^"]+)"`).FindStringSubmatch(page)
	if code != http.StatusOK || !strings.Contains(page, "needs AI") || !strings.Contains(page, "Start it as") || msg == nil {
		t.Fatalf("a question with AI off = %d:\n%s", code, problem(page))
	}
	code, body, h := a.post(c, base+"/tell", url.Values{"message": {msg[1]}, "playbook": {pv}}, nil)
	if code != http.StatusSeeOther || itemID.FindStringSubmatch(h.Get("Location")) == nil {
		t.Errorf("starting it as work = %d to %q: %s", code, h.Get("Location"), problem(body))
	}

	a.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if strings.Contains(r.Instructions, "small search") {
			return `{"kind":"query","processes":[],"status":"open","person":"","role":"","entry":"","period":"","count":true}`, nil
		}
		return `{"kind":"question","fields":{},"new_items":[],"command":"","reason":"","reply":""}`, nil
	}}})
	if _, home, _ := a.get(c, base+"/"); !strings.Contains(home, "You can also ask") {
		t.Error("with AI on, Tell Seyal doesn't suggest asking")
	}
	calls := a.count("SELECT count(*) FROM ai_calls")
	for i := 1; i <= 11; i++ {
		_, page, _ := a.post(c, base+"/tell", url.Values{"playbook": {pv}, "text": {"How many items are open?"}}, nil)
		page = html.UnescapeString(page)
		busy := strings.Contains(page, "asked a lot of questions")
		if busy != (i == 11) || (i < 11 && !strings.Contains(page, "open item")) {
			t.Fatalf("question %d: busy = %v:\n%s", i, busy, problem(page))
		}
	}
	if n := a.count("SELECT count(*) FROM ai_calls"); n != calls+21 {
		t.Errorf("%d AI calls, want 21 (11 readings, 10 searches)", n-calls)
	}
	a.tablesMatchLedger(t)
}

// TestAskNearTheTimeLimit: when the web server's time limit for the request leaves too little time
// for the search call (the first AI call was slow), Seyal doesn't make it. The person gets a page in
// time, saying it couldn't work the question out, with a button to start the message as work.
func TestAskNearTheTimeLimit(t *testing.T) {
	a := newSite(t)
	a.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if strings.Contains(r.Instructions, "small search") {
			return `{"kind":"query","processes":[],"status":"open","person":"","role":"","entry":"","period":"","count":true}`, nil
		}
		return `{"kind":"question","fields":{},"new_items":[],"command":"","reason":"","reply":""}`, nil
	}}})
	// 50 seconds leaves 30 for the answer, less than the search call may take; 15 leaves none.
	for _, limit := range []time.Duration{50 * time.Second, 15 * time.Second} {
		srv, err := server.New(server.Config{App: a.app, Store: a.store, Library: a.lib, AI: a.ai, RequestLimit: limit})
		if err != nil {
			t.Fatal(err)
		}
		a.srv = httptest.NewServer(srv.Handler())
		t.Cleanup(a.srv.Close)
		c := a.logIn(arjun)
		base := "/t/" + a.slug
		_, home, _ := a.get(c, base+"/")
		m := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
		if m == nil {
			t.Fatalf("no process:\n%s", problem(home))
		}
		calls := a.count("SELECT count(*) FROM ai_calls")
		code, page, _ := a.post(c, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"How many items are open?"}}, nil)
		page = html.UnescapeString(page)
		if code != http.StatusOK || !strings.Contains(page, "couldn't work that question out") || !strings.Contains(page, "Start it as") {
			t.Errorf("limit %v: a question = %d:\n%s", limit, code, problem(page))
		}
		if n := a.count("SELECT count(*) FROM ai_calls"); n != calls+1 {
			t.Errorf("limit %v: %d AI calls, want 1 (no search)", limit, n-calls)
		}
	}
	a.tablesMatchLedger(t)
}
