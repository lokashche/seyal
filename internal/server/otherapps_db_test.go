//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/send"
	"seyal/internal/server"
)

// fakeApp stands in for another app (a sheet's script): it keeps every post and answers with code and
// reply. Nothing leaves this computer.
type fakeApp struct {
	mu    sync.Mutex
	posts []fakePost
	code  int
	reply string
}

type fakePost struct {
	address string
	header  http.Header
	body    []byte
}

func (f *fakeApp) PostRedirect(_ context.Context, address string, _ []string, header http.Header, body []byte) (send.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, fakePost{address, header, body})
	return send.Answer{Status: f.code, Body: []byte(f.reply)}, nil
}

// changingApp is a fakeApp that runs during while a post is on its way (an import that changes the
// set-up at that moment).
type changingApp struct {
	*fakeApp
	during func()
}

func (c *changingApp) PostRedirect(ctx context.Context, address string, to []string, header http.Header, body []byte) (send.Answer, error) {
	c.during()
	return c.fakeApp.PostRedirect(ctx, address, to, header, body)
}

func (f *fakeApp) got() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePost(nil), f.posts...)
}

// withSend is the same business on a server whose sending to other apps is on or off, allowing hosts,
// with fake standing in for the outgoing caller.
func (s *site) withSend(t *testing.T, on bool, hosts string, fake send.Poster) *site {
	t.Helper()
	h, err := send.ParseHosts(hosts)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{App: s.app, Store: s.store, Library: s.lib, AI: s.ai,
		SendOn: on, SendHosts: h, Sender: fake})
	if err != nil {
		t.Fatal(err)
	}
	p := *s
	p.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(p.srv.Close)
	return &p
}

// demoHost is the host of the demo's other app (examples/seed/demo.yaml).
const demoHost = "sheet.kaveri-demo.example"

// TestOtherAppsLeaveSeyal: the demo's "Jobs sheet (demo)" in Settings → Processes → Leaves Seyal (D95).
// The admin sends a test to a fake: exactly the fictional data goes, signed with the business's secret,
// and the page shows the data and the answer. A server with sending off, or without the host on its
// list, sends nothing. Pause is a ledger event by the admin and stops tests until Resume. The address
// and the secret are never on any page, in the export or in the report.
func TestOtherAppsLeaveSeyal(t *testing.T) {
	ctx := context.Background()
	plain := newSite(t)
	fake := &fakeApp{code: http.StatusOK, reply: `{"ok":true}`}
	s := plain.withSend(t, true, "hook.make.example, "+demoHost, fake)
	admin := s.logIn(lakshmi)
	processes := "/t/" + s.slug + "/settings/processes"
	target := "/t/" + s.slug + "/settings/other-apps/jobs-sheet"

	var address, secret string
	err := db.WithTenant(ctx, s.app, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT url, secret FROM other_apps WHERE name = 'jobs-sheet'`).Scan(&address, &secret)
	})
	if err != nil {
		t.Fatal(err)
	}
	// secretsGone checks that a page shows neither the address nor the secret, nor a part of either.
	secretsGone := func(what, body string) {
		t.Helper()
		for _, bad := range []string{address, secret, "AAAA0000", "/macros/s/", "whsec_", secret[len("whsec_") : len("whsec_")+12]} {
			if strings.Contains(body, bad) {
				t.Errorf("%s shows %q (the address or the signing secret)", what, bad)
			}
		}
	}
	page := func(c *http.Client, path string) string {
		t.Helper()
		code, body, _ := s.get(c, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		secretsGone(path, body)
		return html.UnescapeString(body)
	}
	has := func(what, body string, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if !strings.Contains(body, p) {
				t.Errorf("%s: no %q on the page", what, p)
			}
		}
	}
	press := func(c *http.Client, do string) (int, string) {
		t.Helper()
		code, body, _ := s.post(c, target, url.Values{"do": {do}}, nil)
		secretsGone("after "+do, body)
		return code, html.UnescapeString(body)
	}

	has("on", page(admin, processes), "Jobs sheet (demo)", "Google (demo) · "+demoHost+" · address ends …/exec", ">on<",
		"may receive: Client, Nature of work, Fee agreed, Advance, Timeline (days)", `/other-apps/jobs-sheet#leaves-seyal"`,
		`aria-label="Send a test to Jobs sheet (demo)"`, `aria-label="Pause sending to Jobs sheet (demo)"`)

	// Send a test: the fake gets the fictional row, signed with the business's secret.
	code, body := press(admin, "test")
	if code != http.StatusOK {
		t.Fatalf("Send a test = %d: %s", code, problem(body))
	}
	has("tested", body, "Test sent to Jobs sheet (demo): answered OK. What went is below, under Leaves Seyal.", // the banner at the top
		"Test sent to Jobs sheet (demo): answered OK.", "TEST-0000 · Acme Traders · ₹0.00",
		`"number": "TEST-0000"`, "Last test at ", "by Lakshmi Narayanan: answered OK")
	posts := fake.got()
	if len(posts) != 1 {
		t.Fatalf("the fake got %d posts, want 1", len(posts))
	}
	p := posts[0]
	if p.address != address {
		t.Error("the test didn't go to the business's address")
	}
	at, _ := strconv.ParseInt(p.header.Get("webhook-timestamp"), 10, 64)
	want, err := send.Sign(secret, p.header.Get("webhook-id"), time.Unix(at, 0), p.body)
	if err != nil || p.header.Get("webhook-signature") != want {
		t.Errorf("the signature doesn't match the business's secret (%v)", err)
	}
	var sent struct {
		Type, Business string
		Test           bool
		Data           struct {
			Number string
			Fields map[string]any
			Row    []any
		}
	}
	if err := json.Unmarshal(p.body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Type != "seyal.test" || !sent.Test || sent.Business != s.slug || sent.Data.Number != "TEST-0000" ||
		len(sent.Data.Fields) != 5 || sent.Data.Fields["client"] != "test" || len(sent.Data.Row) != 6 || sent.Data.Row[1] != "Acme Traders" {
		t.Errorf("the test sent %s", p.body)
	}
	if !strings.HasPrefix(p.header.Get("webhook-id"), "snd_"+s.slug+"_test_") {
		t.Errorf("webhook-id %q", p.header.Get("webhook-id"))
	}

	// The other app is down: the page says so, with its reply code, and so does the line.
	fake.mu.Lock()
	fake.code = http.StatusServiceUnavailable
	fake.mu.Unlock()
	if code, body := press(admin, "test"); code != http.StatusOK || !strings.Contains(body, "Test sent to Jobs sheet (demo): answered 503.") ||
		!strings.Contains(body, `class="callout warn"`) || !strings.Contains(body, "by Lakshmi Narayanan: answered 503") {
		t.Errorf("the other app down: %d %s", code, problem(body))
	}

	// Only admins.
	if code, _ := press(s.logIn(arjun), "test"); code != http.StatusForbidden {
		t.Errorf("Send a test by someone who isn't an admin = %d, want 403", code)
	}
	if code, _ := press(s.logIn(arjun), "pause"); code != http.StatusForbidden {
		t.Errorf("Pause by someone who isn't an admin = %d, want 403", code)
	}
	if code, _, _ := s.post(admin, "/t/"+s.slug+"/settings/other-apps/no-such-app", url.Values{"do": {"test"}}, nil); code != http.StatusNotFound {
		t.Errorf("an app the file doesn't list = %d, want 404", code)
	}

	// Pause: a ledger event by the admin; a test is refused until Resume.
	code, body = press(admin, "pause")
	if code != http.StatusOK {
		t.Fatalf("Pause = %d: %s", code, problem(body))
	}
	has("paused", body, "Paused by Lakshmi Narayanan at ", `aria-label="Resume sending to Jobs sheet (demo)"`)
	before := len(fake.got())
	if code, body := press(admin, "test"); code != http.StatusConflict || !strings.Contains(body, "is paused. Press Resume first") {
		t.Errorf("a test while paused = %d %s", code, problem(body))
	}
	if code, _ := press(admin, "pause"); code != http.StatusOK {
		t.Errorf("a second Pause = %d", code)
	}
	if code, _ := press(admin, "resume"); code != http.StatusOK {
		t.Errorf("Resume = %d", code)
	}
	has("resumed", page(admin, processes), `aria-label="Pause sending to Jobs sheet (demo)"`)
	err = db.WithTenant(ctx, s.app, s.tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT actor_person_id::text, body::jsonb #> '{payload,changes,other_app_paused:jobs-sheet}'
			FROM ledger WHERE kind = 'business.updated' AND body::jsonb #> '{payload,changes}' ? 'other_app_paused:jobs-sheet' ORDER BY seq`)
		if err != nil {
			return err
		}
		type ev struct {
			By     string
			Change map[string]any
		}
		evs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ev])
		if err != nil {
			return err
		}
		if len(evs) != 2 || evs[0].By != s.people[lakshmi] || evs[0].Change["new"] != true ||
			evs[1].Change["new"] != false || evs[1].Change["was_why"] != "admin" {
			t.Errorf("pause events: %+v, want Pause then Resume by Lakshmi (not the second Pause, nor Arjun's)", evs)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// An import gives the app a new secret while a test is on its way: the answer (about the old secret)
	// is not kept as the last test, and the page says to test again.
	setSecret := func(to string) {
		t.Helper()
		if err := db.WithTenant(ctx, s.owner, s.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE other_apps SET secret = $1 WHERE name = 'jobs-sheet'`, to)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	racer := &changingApp{fakeApp: &fakeApp{code: http.StatusOK, reply: `{"ok":true}`},
		during: func() { setSecret("whsec_" + strings.Repeat("B", 43)) }}
	racing := s.withSend(t, true, demoHost, racer)
	code, body, _ = racing.post(racing.logIn(lakshmi), target, url.Values{"do": {"test"}}, nil)
	body = html.UnescapeString(body)
	if code != http.StatusConflict || !strings.Contains(body, "changed while the test was on its way") ||
		strings.Contains(body, ": answered OK") || len(racer.got()) != 1 {
		t.Errorf("a test raced by a new secret = %d %s", code, problem(body))
	}
	setSecret(secret)

	// A server whose host list doesn't have the app's host, or with sending off, sends nothing.
	for _, other := range []struct {
		name  string
		site  *site
		says  string
		state string
	}{
		{"host not listed", plain.withSend(t, true, "hook.make.example", fake), "may not send to " + demoHost, "not allowed on this server"},
		{"sending off", plain.withSend(t, false, demoHost, fake), "switched off on this Seyal server (SEYAL_SEND)", "off on this server"},
	} {
		a := other.site.logIn(lakshmi)
		code, body, _ := other.site.post(a, target, url.Values{"do": {"test"}}, nil)
		if code != http.StatusConflict || !strings.Contains(html.UnescapeString(body), other.says) {
			t.Errorf("%s: Send a test = %d %s", other.name, code, problem(body))
		}
		_, list, _ := other.site.get(a, processes)
		has(other.name, list, other.state)
	}

	// The host is listed but the host its answer redirects to isn't: the line names the missing host,
	// and a test is refused before any call (it would reach the app, then look like a failure).
	if err := db.WithTenant(ctx, s.owner, s.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE other_apps SET redirect_to = '{redirect.kaveri-demo.example}' WHERE name = 'jobs-sheet'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a := s.logIn(lakshmi)
	if code, body, _ := s.post(a, target, url.Values{"do": {"test"}}, nil); code != http.StatusConflict ||
		!strings.Contains(html.UnescapeString(body), "may not send to redirect.kaveri-demo.example") {
		t.Errorf("redirect host not listed: Send a test = %d %s", code, problem(body))
	}
	has("redirect host not listed", page(a, processes), "not allowed on this server",
		"redirect.kaveri-demo.example isn't in this server's SEYAL_SEND_HOSTS")
	if n := len(fake.got()); n != before {
		t.Errorf("the fake got %d more posts while paused, unlisted or switched off; want none", n-before)
	}

	// The address and the secret are on no page, nor in the export or the report.
	a = s.logIn(arjun)
	_, home, _ := s.get(a, "/t/"+s.slug+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	_, _, h := s.post(a, "/t/"+s.slug+"/tell", url.Values{"playbook": {pv}, "text": {"Books for Sundaram"}}, nil)
	item := h.Get("Location")
	for _, path := range []string{item, item + "/report", "/t/" + s.slug + "/", "/t/" + s.slug + "/me",
		processes, "/t/" + s.slug + "/settings/processes/job-evaluation", "/t/" + s.slug + "/export/job-evaluation",
		"/t/" + s.slug + "/settings/people/export", "/t/" + s.slug + "/settings/ai"} {
		c := admin
		if strings.HasPrefix(path, item) {
			c = a
		}
		code, body, _ := s.get(c, path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d", path, code)
		}
		secretsGone(path, body)
	}
}
