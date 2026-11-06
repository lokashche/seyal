//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"seyal/internal/db"
	"seyal/internal/notify"
	"seyal/internal/push"
	"seyal/internal/send"
	"seyal/internal/server"
	"seyal/internal/setup"
)

// fakePushService stands in for the push services (Google, Apple, Mozilla, Microsoft): it keeps what
// each device push address was sent, as the phone's service worker would read it, and answers 201.
// Nothing leaves this computer.
type fakePushService struct {
	mu   sync.Mutex
	got  []buzz
	logs strings.Builder // the reader's log
}

type buzz struct {
	endpoint, body, navigate string
	at                       time.Time
}

func (f *fakePushService) Send(_ context.Context, d push.Device, m push.Message) (send.Answer, error) {
	var p struct {
		Notification struct{ Body, Navigate string } `json:"notification"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return send.Answer{Status: http.StatusBadRequest}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, buzz{endpoint: d.Endpoint, body: p.Notification.Body, navigate: p.Notification.Navigate, at: time.Now()})
	return send.Answer{Status: http.StatusCreated}, nil
}

func (f *fakePushService) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs.Write(b)
}

// wait waits up to 10 seconds for a buzz to endpoint whose line starts with prefix.
func (f *fakePushService) wait(endpoint, prefix string) (buzz, bool) {
	for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(50 * time.Millisecond) {
		if b, ok := f.find(endpoint, prefix); ok {
			return b, true
		}
	}
	return buzz{}, false
}

// find returns the first buzz to endpoint whose line starts with prefix.
func (f *fakePushService) find(endpoint, prefix string) (buzz, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.got {
		if b.endpoint == endpoint && strings.HasPrefix(b.body, prefix) {
			return b, true
		}
	}
	return buzz{}, false
}

// TestPassBuzzesWithinSeconds: the whole way, with the real web server and the real phone-note reader
// (only the push service is a fake). Priya turns phone notes on, on Me. Arjun sends a new job to
// Operations, Ben takes it and passes it to Priya. Every logged-in POST pokes the reader, so Priya's
// phone gets "New work from Ben: J-…" within seconds, not at the next minute's pass, and a tap opens
// the item.
func TestPassBuzzesWithinSeconds(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	ctx := context.Background()
	var err error
	if s.passwords["ben"], err = setup.NewPassword(ctx, s.app, s.mail("ben"), s.tenant); err != nil {
		t.Fatal(err)
	}
	// The business's set-up file turns phone notes on, at every hour (the import writes these; the app
	// may only read them).
	if _, err := s.owner.Exec(ctx, `UPDATE tenants SET notifications = true, notify_from = '00:00', notify_until = '24:00',
		days_off = '{}', holidays = '{}' WHERE id = $1`, s.tenant); err != nil {
		t.Fatal(err)
	}
	fake := &fakePushService{}
	reader := notify.New(s.app, log.New(fake, "", 0), fake)
	reader.List = func(context.Context) ([]db.Tenant, error) { return []db.Tenant{{ID: s.tenant, Slug: s.slug}}, nil }
	keys, err := push.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{App: s.app, Store: s.store, Library: s.lib, AI: s.ai, PushKey: keys.Public(), Poke: reader.Poke})
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(s.srv.Close)
	// On a server the reader has run every minute since it started; its first pass sets where it
	// starts reading this business's ledger (from now on: old work tells nobody).
	reader.Pass(ctx)
	runCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		reader.Run(runCtx, time.Hour) // the minute's pass never comes in this test: only pokes
	}()
	defer func() {
		stop()
		<-stopped
		if fake.logs.Len() > 0 {
			t.Errorf("the reader logged: %s", fake.logs.String())
		}
	}()
	base := "/t/" + s.slug

	// Priya turns phone notes on for her phone.
	p := s.logIn(priya)
	sub := browserSub(t)
	if code, got := s.postJSON(p, base+"/push/devices", sub, nil); code != http.StatusOK || got["device"] == "" {
		t.Fatalf("Priya's turn on = %d %v", code, got)
	}
	endpoint := sub["endpoint"].(string)

	// Arjun sends a new job to Operations; Ben takes it.
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
	// Priya's phone tells her first, within seconds: the job is with her team.
	first, ok := fake.wait(endpoint, "New work for ")
	if !ok {
		t.Fatal("Priya's phone got nothing about the job sent to Operations in 10 seconds")
	}
	t.Logf("Priya's phone shows %q", first.body)
	b := s.logIn("ben")
	if code, body, _ := s.post(b, job+"/take", url.Values{"holder": {"team:ops"}, "via": {"button"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Ben's Take = %d: %s", code, problem(body))
	}

	// Ben passes it to Priya.
	start := time.Now()
	if code, body, _ := s.post(b, job+"/pass", url.Values{"to": {"person:" + s.people[priya]}, "holder": {"person:" + s.people["ben"]}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Ben's Pass on = %d: %s", code, problem(body))
	}
	got, ok := fake.wait(endpoint, "New work from Ben: ")
	if !ok {
		t.Fatal("Priya's phone got nothing from Ben's hand-over in 10 seconds")
	}
	took := got.at.Sub(start)
	t.Logf("Priya's phone shows %q, %.1f seconds after Ben pressed Pass on; a tap opens %s", got.body, took.Seconds(), got.navigate)
	if took > 5*time.Second {
		t.Errorf("it took %s; want within seconds", took)
	}
	if !regexp.MustCompile(`^New work from Ben: J-\d{4}-\d{4}$`).MatchString(got.body) || strings.Contains(got.body, "Sundaram") {
		t.Errorf("the lock screen line is %q", got.body)
	}
	if !strings.HasSuffix(got.navigate, base+"/items/"+id) {
		t.Errorf("a tap opens %q; want the item", got.navigate)
	}
}
