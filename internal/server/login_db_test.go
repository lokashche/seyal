//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/setup"
)

// join adds the login of name on site s to the business of other, as the server's operator would:
// other's admin adds a person with that email, and the operator links the login to them. It
// returns the person's id in other.
func (s *site) join(other *site, name, role string) string {
	s.t.Helper()
	ctx := context.Background()
	code, body, _ := other.post(other.logIn(lakshmi), "/t/"+other.slug+"/settings/people",
		url.Values{"name": {"Joined " + name}, "email": {s.mail(name)}, "role": {role}}, nil)
	if code != http.StatusOK {
		s.t.Fatalf("adding %s to %s = %d: %s", name, other.slug, code, problem(body))
	}
	var personID string
	err := db.WithTenant(ctx, s.app, other.tenant, func(tx pgx.Tx) error {
		var accountID string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM accounts WHERE email = $1", s.mail(name)).Scan(&accountID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT id::text FROM persons WHERE login_email = $1", s.mail(name)).Scan(&personID); err != nil {
			return err
		}
		return auth.Link(ctx, tx, accountID, personID, "operator", ledger.SystemActor)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return personID
}

// cookie returns the Set-Cookie header for name, or "".
func cookie(h http.Header, name string) string {
	for _, c := range h.Values("Set-Cookie") {
		if strings.HasPrefix(c, name+"=") {
			return c
		}
	}
	return ""
}

// TestOneLoginTwoBusinesses: one login reaches a person in each of several businesses; "/" chooses
// between them; logging out anywhere logs out everywhere.
func TestOneLoginTwoBusinesses(t *testing.T) {
	a, b, third := newSite(t), newSite(t), newSite(t)
	inA, inB, inC := "/t/"+a.slug+"/", "/t/"+b.slug+"/", "/t/"+third.slug+"/"
	joined := a.join(b, arjun, "sales") // Arjun's person in b
	a.join(third, arjun, "sales")       // so that two stay open when one is disabled
	logInAtRoot := func() *http.Client {
		t.Helper()
		c := a.browser()
		code, _, h := a.post(c, "/", url.Values{"email": {a.mail(arjun)}, "password": {a.passwords[arjun]}}, nil)
		if code != http.StatusSeeOther || h.Get("Location") != "/" {
			t.Fatalf("log in on / = %d to %q, want / (which chooses)", code, h.Get("Location"))
		}
		return c
	}

	t.Run("several businesses and none used yet on this device: a list", func(t *testing.T) {
		c := logInAtRoot()
		code, page, _ := a.get(c, "/")
		if code != http.StatusOK || !strings.Contains(page, `href="`+inA+`"`) || !strings.Contains(page, `href="`+inB+`"`) ||
			!strings.Contains(page, `href="`+inC+`"`) {
			t.Errorf("/ = %d, want links to all three businesses:\n%s", code, page)
		}
		if strings.Contains(page, `rel="manifest"`) {
			t.Error("the list on / points at a business's manifest")
		}
	})

	t.Run("one login opens both, and / goes to the last one used here", func(t *testing.T) {
		c := logInAtRoot()
		for _, home := range []string{inA, inB} {
			code, page, h := a.get(c, home)
			if code != http.StatusOK || !strings.Contains(page, "My Work") {
				t.Fatalf("%s = %d", home, code)
			}
			if last := cookie(h, "seyal_last"); !strings.Contains(last, "seyal_last="+strings.Trim(home[3:], "/")) || !strings.Contains(last, "Path=/;") {
				t.Errorf("after %s, seyal_last = %q", home, last)
			}
		}
		if code, _, h := a.get(c, "/"); code != http.StatusSeeOther || h.Get("Location") != inB {
			t.Errorf("/ = %d to %q, want the last one used (%s)", code, h.Get("Location"), inB)
		}
		if code, _, h := a.get(c, inA+"login"); code != http.StatusSeeOther || h.Get("Location") != inA {
			t.Errorf("the login page while logged in = %d to %q, want into the business", code, h.Get("Location"))
		}
		if code, _, h := a.get(c, "/t/no-such-firm/login"); code != http.StatusSeeOther || h.Get("Location") != "/" {
			t.Errorf("a made-up address's login page while logged in = %d to %q, want /", code, h.Get("Location"))
		}
	})

	t.Run("logging out in one business logs out in the other too", func(t *testing.T) {
		c := logInAtRoot()
		a.get(c, inA)
		a.get(c, inB)
		if code, _, _ := a.post(c, inA+"logout", nil, nil); code != http.StatusSeeOther {
			t.Fatalf("logout = %d", code)
		}
		if code, _, _ := a.get(c, inB); code != http.StatusSeeOther {
			t.Errorf("the other business after logging out = %d, want a redirect to login", code)
		}
	})

	t.Run("a last-used business the login isn't in: the list", func(t *testing.T) {
		c := logInAtRoot()
		u, _ := url.Parse(a.srv.URL)
		c.Jar.SetCookies(u, []*http.Cookie{{Name: "seyal_last", Value: "no-such-firm", Path: "/"}})
		if code, page, h := a.get(c, "/"); code != http.StatusOK || !strings.Contains(page, `href="`+inA+`"`) {
			t.Errorf("/ = %d to %q, want the list", code, h.Get("Location"))
		}
	})

	t.Run("a business someone was disabled in is never opened from /", func(t *testing.T) {
		c := logInAtRoot()
		a.get(c, inB) // the last one used
		code, body, _ := b.post(b.logIn(lakshmi), inB+"settings/people/"+joined,
			url.Values{"name": {"Joined arjun"}, "email": {a.mail(arjun)}, "status": {"disabled"}}, nil)
		if code != http.StatusOK {
			t.Fatalf("disable = %d: %s", code, problem(body))
		}
		code, _, h := a.get(c, inB)
		if code != http.StatusSeeOther || h.Get("Location") != inB+"login" || cookie(h, "seyal_last") != "" {
			t.Errorf("the disabled business = %d to %q (cookie %q)", code, h.Get("Location"), cookie(h, "seyal_last"))
		}
		if code, _, h = a.get(c, inB+"login"); code != http.StatusSeeOther || h.Get("Location") != "/" {
			t.Errorf("its login page = %d to %q, want /", code, h.Get("Location"))
		}
		code, page, h := a.get(c, "/")
		if code != http.StatusOK || strings.Contains(page, `href="`+inB+`"`) || !strings.Contains(page, `href="`+inA+`"`) ||
			!strings.Contains(page, `href="`+inC+`"`) {
			t.Errorf("/ = %d to %q, want the list of the two businesses still open:\n%s", code, h.Get("Location"), page)
		}
		if code, _, _ = a.get(c, inA); code != http.StatusOK {
			t.Errorf("a business still open = %d", code)
		}
	})

	a.tablesMatchLedger(t)
	b.tablesMatchLedger(t)
	third.tablesMatchLedger(t)
}

func TestLanding(t *testing.T) {
	s := newSite(t)
	home := "/t/" + s.slug + "/"

	t.Run("one business: straight in", func(t *testing.T) {
		c := s.browser()
		_, _, h := s.post(c, "/", url.Values{"email": {s.mail(priya)}, "password": {s.passwords[priya]}}, nil)
		if h.Get("Location") != "/" {
			t.Fatalf("log in on / to %q", h.Get("Location"))
		}
		if code, _, h := s.get(c, "/"); code != http.StatusSeeOther || h.Get("Location") != home {
			t.Errorf("/ = %d to %q, want %s", code, h.Get("Location"), home)
		}
	})

	t.Run("a business only on this device's last-used cookie isn't opened", func(t *testing.T) {
		c := s.logIn(priya)
		u, _ := url.Parse(s.srv.URL)
		c.Jar.SetCookies(u, []*http.Cookie{{Name: "seyal_last", Value: "no-such-firm", Path: "/"}})
		if code, _, h := s.get(c, "/"); code != http.StatusSeeOther || h.Get("Location") != home {
			t.Errorf("/ = %d to %q, want %s", code, h.Get("Location"), home)
		}
		if _, _, h := s.get(s.logIn(priya), "/t/no-such-firm/"); cookie(h, "seyal_last") != "" {
			t.Error("a made-up address was remembered as the last business")
		}
	})

	t.Run("no business yet: a short message and a way to log out", func(t *testing.T) {
		email := "nobody-" + strings.ToLower(rand.Text()[:8]) + "@" + s.slug + ".kaveri-demo.example"
		password, err := setup.NewPassword(context.Background(), s.app, email, "")
		if err != nil {
			t.Fatal(err)
		}
		c := s.browser()
		if _, _, h := s.post(c, home+"login", url.Values{"email": {email}, "password": {password}}, nil); h.Get("Location") != "/" {
			t.Fatalf("log in = %q, want /", h.Get("Location"))
		}
		code, page, _ := s.get(c, "/")
		if code != http.StatusOK || !strings.Contains(page, "any business on this Seyal server yet") || !strings.Contains(page, `action="/logout"`) {
			t.Fatalf("/ = %d:\n%s", code, page)
		}
		if code, _, h := s.get(c, home); code != http.StatusSeeOther || h.Get("Location") != home+"login" {
			t.Errorf("a business the login isn't in = %d to %q", code, h.Get("Location"))
		}
		code, _, h := s.post(c, "/logout", nil, nil)
		if code != http.StatusSeeOther || h.Get("Location") != "/" || !strings.Contains(cookie(h, "seyal_account"), "Max-Age=0") {
			t.Errorf("log out on / = %d to %q, cookie %q", code, h.Get("Location"), cookie(h, "seyal_account"))
		}
		if _, page, _ := s.get(c, "/"); !strings.Contains(page, `name="password"`) {
			t.Error("after logging out, / isn't the login form")
		}
	})

	t.Run("a temporary password: the password page and logging out, no loop", func(t *testing.T) {
		s.temporaryPassword(arjun)
		c := s.browser()
		_, _, h := s.post(c, "/", url.Values{"email": {s.mail(arjun)}, "password": {s.passwords[arjun]}}, nil)
		if code, _, h2 := s.get(c, h.Get("Location")); code != http.StatusSeeOther || h2.Get("Location") != home {
			t.Fatalf("/ = %d to %q", code, h2.Get("Location"))
		}
		if code, _, h := s.get(c, home); code != http.StatusSeeOther || h.Get("Location") != home+"password" {
			t.Errorf("My Work = %d to %q, want the password page", code, h.Get("Location"))
		}
		if code, _, _ := s.get(c, home+"password"); code != http.StatusOK {
			t.Errorf("the password page = %d", code)
		}
		if code, _, h := s.post(c, home+"logout", nil, nil); code != http.StatusSeeOther || h.Get("Location") != home+"login" {
			t.Errorf("logout = %d to %q", code, h.Get("Location"))
		}
	})
}

// oldSession starts a session for personID the way the old per-business login did, puts its cookie
// for this site's business address in browser c, and returns its token. (A cookie jar ignores the
// port, so every test site's server sees the cookie.)
func (s *site) oldSession(c *http.Client, personID string) string {
	s.t.Helper()
	token := rand.Text()
	sum := sha256.Sum256([]byte(token))
	err := db.WithTenant(context.Background(), s.owner, s.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO sessions (token_sha256, tenant_id, person_id, expires_at)
			VALUES ($1, current_tenant(), $2, $3)`, hex.EncodeToString(sum[:]), personID, time.Now().Add(time.Hour))
		return err
	})
	if err != nil {
		s.t.Fatal(err)
	}
	home := "/t/" + s.slug + "/"
	u, _ := url.Parse(s.srv.URL + home)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "seyal_session", Value: token, Path: home}})
	return token
}

// TestOldCookies: for one release (D64), a browser still holding a business address's old login
// cookie is moved to the new login cookie without logging in again, if the person has a login.
func TestOldCookies(t *testing.T) {
	s := newSite(t)
	home := "/t/" + s.slug + "/"

	t.Run("a person with a login stays logged in", func(t *testing.T) {
		c := s.browser()
		token := s.oldSession(c, s.people[arjun])
		code, page, h := s.get(c, home)
		if code != http.StatusOK || !strings.Contains(page, "Arjun Mehta") {
			t.Fatalf("home with the old cookie = %d", code)
		}
		if !strings.HasPrefix(cookie(h, "seyal_account"), "seyal_account=") || strings.Contains(cookie(h, "seyal_account"), "Max-Age=0") ||
			!strings.Contains(cookie(h, "seyal_session"), "Path="+home+"; Max-Age=0") {
			t.Errorf("cookies = %v; want the new one set and the old one removed", h.Values("Set-Cookie"))
		}
		if code, _, _ := s.get(c, home+"me"); code != http.StatusOK {
			t.Errorf("the next page = %d", code)
		}
		again := s.browser()
		u, _ := url.Parse(s.srv.URL + home)
		again.Jar.SetCookies(u, []*http.Cookie{{Name: "seyal_session", Value: token, Path: home}})
		if code, _, h := s.get(again, home); code != http.StatusSeeOther || cookie(h, "seyal_account") != "" {
			t.Errorf("the old cookie a second time = %d (%v); it was used up", code, h.Values("Set-Cookie"))
		}
	})

	t.Run("a person without a login logs in again, also after being linked", func(t *testing.T) {
		l := s.logIn(lakshmi)
		code, body, _ := s.post(l, home+"settings/people", url.Values{"name": {"Neha Joshi"}, "email": {s.mail("neha")}, "role": {"sales"}}, nil)
		if code != http.StatusOK {
			t.Fatalf("add person = %d: %s", code, problem(body))
		}
		neha := s.personID(t, "neha")
		c := s.browser()
		s.oldSession(c, neha)
		if code, _, h := s.get(c, home); code != http.StatusSeeOther || h.Get("Location") != home+"login" || cookie(h, "seyal_account") != "" {
			t.Errorf("an unlinked person's old cookie = %d to %q", code, h.Get("Location"))
		}
		// The operator links her: a session from before the link is never swapped for her login's.
		if _, err := setup.NewPassword(context.Background(), s.app, s.mail("neha"), s.tenant); err != nil {
			t.Fatal(err)
		}
		if code, _, h := s.get(c, home); code != http.StatusSeeOther || cookie(h, "seyal_account") != "" {
			t.Errorf("the old cookie after the link = %d (%v), want logged out", code, h.Values("Set-Cookie"))
		}
		if n := s.count("SELECT count(*) FROM ledger WHERE kind = 'person.login_set' AND subject_id = $1", neha); n != 1 {
			t.Errorf("person.login_set events for her here = %d, want 1", n)
		}
	})

	s.tablesMatchLedger(t)
}

// TestOldCookiesLogOut: for one release (D64), logging out also ends the old per-business sessions of
// the login's people, so an old cookie for another business address can't log the device back in.
func TestOldCookiesLogOut(t *testing.T) {
	a, b := newSite(t), newSite(t)
	inA, inB := "/t/"+a.slug+"/", "/t/"+b.slug+"/"
	joined := a.join(b, arjun, "sales")
	loggedOut := func(c *http.Client) {
		t.Helper()
		for _, home := range []string{inB, inA} {
			if code, _, h := a.get(c, home); code != http.StatusSeeOther || cookie(h, "seyal_account") != "" {
				t.Errorf("%s after logging out = %d (%v), want logged out", home, code, h.Values("Set-Cookie"))
			}
		}
	}

	t.Run("old cookies for two businesses, then log out in one", func(t *testing.T) {
		c := a.browser()
		a.oldSession(c, a.people[arjun])
		b.oldSession(c, joined)
		if code, _, _ := a.get(c, inA); code != http.StatusOK {
			t.Fatalf("with the old cookie = %d", code)
		}
		if code, _, h := a.post(c, inA+"logout", nil, nil); code != http.StatusSeeOther || h.Get("Location") != inA+"login" {
			t.Fatalf("logout = %d to %q", code, h.Get("Location"))
		}
		loggedOut(c)
	})

	t.Run("an old cookie, then a new login and logging out on /", func(t *testing.T) {
		c := a.browser()
		a.oldSession(c, a.people[arjun])
		if code, _, _ := a.post(c, "/", url.Values{"email": {a.mail(arjun)}, "password": {a.passwords[arjun]}}, nil); code != http.StatusSeeOther {
			t.Fatalf("log in on / = %d", code)
		}
		if code, _, h := a.post(c, "/logout", nil, nil); code != http.StatusSeeOther || h.Get("Location") != "/" {
			t.Fatalf("log out on / = %d to %q", code, h.Get("Location"))
		}
		loggedOut(c)
	})

	a.tablesMatchLedger(t)
	b.tablesMatchLedger(t)
}
