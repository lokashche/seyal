//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/server"
	"seyal/internal/setup"
)

// linkCode returns the code of the link shown on a page.
func linkCode(t *testing.T, page string) string {
	t.Helper()
	m := shownLink.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no link on the page:\n%s", problem(page))
	}
	return m[1]
}

// addWithLink has the admin of s add a person with this email and returns the link's code.
func (s *site) addWithLink(admin *http.Client, name, email, role string) string {
	s.t.Helper()
	status, page, _ := s.post(admin, "/t/"+s.slug+"/settings/people", url.Values{"name": {name}, "email": {email}, "role": {role}}, nil)
	if status != http.StatusOK {
		s.t.Fatalf("adding %s = %d: %s", name, status, problem(page))
	}
	return linkCode(s.t, page)
}

// personWith returns the id of the person with this login email in s.
func personWith(t *testing.T, s *site, email string) string {
	t.Helper()
	var id string
	if err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT id::text FROM persons WHERE login_email = $1", email).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// setUp reports whether the person with this email is shown as set up (linked to a login) in Settings.
func (s *site) setUp(admin *http.Client, email string) bool {
	s.t.Helper()
	_, people, _ := s.get(admin, "/t/"+s.slug+"/settings/people")
	i := strings.Index(people, email)
	if i < 0 {
		s.t.Fatalf("%s isn't on the People page", email)
	}
	row := people[strings.LastIndex(people[:i], "<li"):]
	return !strings.Contains(row[:strings.Index(row, "</li>")], "not set up yet")
}

// TestJoinLinks: a link for someone whose email already has a login used in another business only
// adds this business to that login (D64). It asks for the login's password, or one tap when the
// browser is already logged in with it, and never changes the password or ends a session.
func TestJoinLinks(t *testing.T) {
	a, b := newSite(t), newSite(t)
	inA, inB := "/t/"+a.slug+"/", "/t/"+b.slug+"/"
	lb := b.logIn(lakshmi)
	victim := a.mail(priya)
	pa := a.logIn(priya) // the victim, logged in to a
	code := b.addWithLink(lb, "Priya Raman", victim, "ops")
	join := func(c *http.Client, code, password string) (int, string, http.Header) {
		return b.post(c, inB+"welcome/"+code, url.Values{"mode": {"join"}, "email": {victim}, "password": {password}}, nil)
	}

	t.Run("the admin's message is the same as for anyone new", func(t *testing.T) {
		_, page, _ := b.get(lb, inB+"settings/people")
		if strings.Contains(page, "already has a Seyal login") {
			t.Error("the People page tells b that the email uses Seyal elsewhere")
		}
	})

	t.Run("the page asks for the login's password, not a new one", func(t *testing.T) {
		status, page, _ := b.get(b.browser(), inB+"welcome/"+code)
		page = html.UnescapeString(page)
		want := "You already have a Seyal login (" + victim + "). Enter its password to add Kaveri Associates (demo)."
		if status != http.StatusOK || !strings.Contains(page, want) || !strings.Contains(page, `autocomplete="current-password"`) ||
			strings.Contains(page, `name="next"`) {
			t.Errorf("join page = %d:\n%s", status, page)
		}
	})

	t.Run("the admin can make a new join link (e.g. the first one expired)", func(t *testing.T) {
		status, page, _ := b.post(lb, inB+"settings/people/"+personWith(t, b, victim)+"/link", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("new link = %d: %s", status, problem(page))
		}
		next := linkCode(t, page)
		if _, welcome, _ := b.get(b.browser(), inB+"welcome/"+next); !strings.Contains(welcome, "Enter its password to add") {
			t.Errorf("the new link should join:\n%s", welcome)
		}
		if status, _, _ := b.get(b.browser(), inB+"welcome/"+code); status != http.StatusNotFound {
			t.Errorf("the first link = %d, want 404 (replaced)", status)
		}
		code = next
	})

	t.Run("a wrong password: 401, and nothing changes", func(t *testing.T) {
		c := b.browser()
		status, page, h := join(c, code, "not-her-password")
		if status != http.StatusUnauthorized || !strings.Contains(page, "Wrong password.") || cookie(h, "seyal_account") != "" {
			t.Errorf("join with a wrong password = %d (cookie %q)", status, cookie(h, "seyal_account"))
		}
		if b.setUp(lb, victim) {
			t.Error("the person was linked after a wrong password")
		}
		a.logIn(priya) // her password is unchanged
	})

	t.Run("an empty password: refused before it is checked, and not counted", func(t *testing.T) {
		for range 11 { // more than the limit: none of them is counted
			status, page, _ := join(b.browser(), code, "")
			if status != http.StatusUnprocessableEntity || !strings.Contains(page, "Please enter your password.") {
				t.Fatalf("join with no password = %d: %s", status, problem(page))
			}
		}
		if b.setUp(lb, victim) {
			t.Error("the person was linked with no password")
		}
	})

	t.Run("choosing a password on a join link: open the link again", func(t *testing.T) {
		status, page, _ := b.post(b.browser(), inB+"welcome/"+code,
			url.Values{"mode": {"choose"}, "next": {"taken-over-pass-1"}, "again": {"taken-over-pass-1"}}, nil)
		if status != http.StatusConflict || !strings.Contains(html.UnescapeString(page), "Please open the link again.") {
			t.Errorf("choose on a join link = %d:\n%s", status, page)
		}
		a.logIn(priya)
	})

	t.Run("logged in with another login: log out first", func(t *testing.T) {
		other := b.logIn(arjun)
		_, page, _ := b.get(other, inB+"welcome/"+code)
		if !strings.Contains(page, "This link is for another login. Log out first.") || strings.Contains(page, victim) ||
			strings.Contains(page, `name="password"`) {
			t.Errorf("join page for another login:\n%s", page)
		}
		status, _, h := join(other, code, a.passwords[priya])
		if status != http.StatusConflict || cookie(h, "seyal_account") != "" {
			t.Errorf("join while logged in as someone else = %d, want 409 and no new login", status)
		}
	})

	t.Run("the right password joins; the password and the other sessions stay", func(t *testing.T) {
		c := b.browser()
		status, page, h := join(c, code, a.passwords[priya])
		if status != http.StatusSeeOther || h.Get("Location") != inB {
			t.Fatalf("join = %d to %q:\n%s", status, h.Get("Location"), page)
		}
		if status, _, _ := b.get(c, inB); status != http.StatusOK {
			t.Errorf("b after joining = %d", status)
		}
		if status, _, _ := a.get(c, inA); status != http.StatusOK {
			t.Errorf("a with the same login = %d", status)
		}
		if status, _, _ := a.get(pa, inA); status != http.StatusOK {
			t.Errorf("her session in a from before = %d; a join ends no session", status)
		}
		a.logIn(priya) // the same password
		if !b.setUp(lb, victim) {
			t.Error("not shown as set up after joining")
		}
		if status, _, _ := join(b.browser(), code, a.passwords[priya]); status != http.StatusNotFound {
			t.Errorf("the link a second time = %d, want 404", status)
		}
	})

	t.Run("one tap when already logged in with the login; Me switches between them", func(t *testing.T) {
		c3 := newSite(t)
		inC := "/t/" + c3.slug + "/"
		code := c3.addWithLink(c3.logIn(lakshmi), "Priya Raman", victim, "ops")
		_, page, _ := c3.get(pa, inC+"welcome/"+code)
		if !strings.Contains(html.UnescapeString(page), "You're logged in as "+victim) || !strings.Contains(page, "Add Kaveri Associates") ||
			strings.Contains(page, `name="password"`) {
			t.Errorf("join page when logged in with the login:\n%s", page)
		}
		status, _, h := c3.post(pa, inC+"welcome/"+code, url.Values{"mode": {"join"}}, nil)
		if status != http.StatusSeeOther || h.Get("Location") != inC {
			t.Fatalf("one tap = %d to %q", status, h.Get("Location"))
		}
		if status, _, _ := c3.get(pa, inC); status != http.StatusOK {
			t.Errorf("the new business with the same cookie = %d", status)
		}
		_, me, _ := a.get(pa, inA+"me")
		// Switching is a full page load (hx-boost off), so the page gets that business's manifest.
		for _, want := range []string{"Your businesses", `href="` + inB + `" hx-boost="false"`, `href="` + inC + `" hx-boost="false"`,
			"here now", victim, "Your login works in every business you belong to.", "in every business you belong to"} {
			if !strings.Contains(me, want) {
				t.Errorf("Me is missing %q", want)
			}
		}
		_, switcher, _ := strings.Cut(me, `aria-label="Your businesses"`)
		if switcher, _, _ = strings.Cut(switcher, "</section>"); strings.Contains(switcher, `href="`+inA+`"`) {
			t.Error("Me links to the business it is in, instead of marking it")
		}
		for _, home := range []string{inB, inC, inA} { // switching needs no new login
			if status, _, _ := a.get(pa, home); status != http.StatusOK {
				t.Errorf("switch to %s = %d", home, status)
			}
		}
		_, me, _ = b.get(b.logIn(arjun), inB+"me")
		if strings.Contains(me, "Your businesses") || strings.Contains(me, "Your login works in every business") {
			t.Error("a login in one business is shown a switcher")
		}
		if !strings.Contains(html.UnescapeString(me), "changes your name and roles in Settings → People.") {
			t.Error("Me doesn't say where an admin changes the name and roles")
		}
		c3.tablesMatchLedger(t)
	})

	a.tablesMatchLedger(t)
	b.tablesMatchLedger(t)
}

// proxied runs s's web app again, as if behind a proxy and with a failed-login count of its own, and
// returns a way to post a form to it from a given visitor's address (X-Forwarded-For). It may be
// called from several goroutines at once.
func (s *site) proxied(t *testing.T) func(path string, form url.Values, ip string) (int, string) {
	t.Helper()
	srv, err := server.New(server.Config{App: s.app, Store: s.store, Library: s.lib, AI: s.ai, BehindProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return func(path string, form url.Values, ip string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := s.browser().Do(req)
		if err != nil {
			t.Error(err) // not Fatal: this may run in another goroutine
			return 0, ""
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
}

// TestJoinRateLimit: wrong passwords on a join link count with the login form's, per visitor and per
// email (the link's, never the one typed), so a link can't be used to guess someone's password
// faster. Each case has its own count (a fresh server) and its own visitors' addresses, so only the
// key it tests can block.
func TestJoinRateLimit(t *testing.T) {
	a, b := newSite(t), newSite(t)
	lb := b.logIn(lakshmi)
	victim := a.mail(priya)
	priyas := "/t/" + b.slug + "/welcome/" + b.addWithLink(lb, "Priya Raman", victim, "ops")
	guess := func(typed string, i int) url.Values {
		return url.Values{"mode": {"join"}, "email": {typed}, "password": {fmt.Sprint("guess number ", i)}}
	}

	t.Run("per email: the link's, whatever is typed, from any address", func(t *testing.T) {
		post := b.proxied(t)
		for i := range 10 {
			if status, _ := post(priyas, guess(b.mail(fmt.Sprint("decoy", i)), i), fmt.Sprintf("192.0.2.%d", i+1)); status != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d, want 401", i+1, status)
			}
		}
		status, page := post(priyas, url.Values{"mode": {"join"}, "password": {a.passwords[priya]}}, "192.0.2.100")
		if status != http.StatusTooManyRequests || !strings.Contains(page, "Too many failed attempts") {
			t.Errorf("the 11th try, with the right password = %d, want 429", status)
		}
		if status, _ := post("/", url.Values{"email": {victim}, "password": {a.passwords[priya]}}, "192.0.2.101"); status != http.StatusTooManyRequests {
			t.Errorf("the login form, from another address = %d, want 429 (the same count)", status)
		}
	})

	t.Run("per visitor: one address guessing at two links", func(t *testing.T) {
		post := b.proxied(t)
		links := []string{
			"/t/" + b.slug + "/welcome/" + b.addWithLink(lb, "Arjun Mehta", a.mail(arjun), "sales"),
			"/t/" + b.slug + "/welcome/" + b.addWithLink(lb, "Lakshmi Narayanan", a.mail(lakshmi), "sales"),
		}
		for i := range 10 { // five per email: no email is blocked
			if status, _ := post(links[i%2], guess("", i), "198.51.100.7"); status != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d, want 401", i+1, status)
			}
		}
		right := url.Values{"mode": {"join"}, "password": {a.passwords[arjun]}}
		if status, _ := post(links[0], right, "198.51.100.7"); status != http.StatusTooManyRequests {
			t.Errorf("the 11th try from the same address = %d, want 429", status)
		}
		if status, _ := post(links[0], right, "198.51.100.8"); status != http.StatusSeeOther {
			t.Errorf("the right password from another address = %d, want 303 (only the visitor was blocked)", status)
		}
	})

	t.Run("guesses sent at the same moment are counted before they are checked", func(t *testing.T) {
		post := b.proxied(t)
		statuses := make([]int, 30)
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Go(func() { statuses[i], _ = post(priyas, guess("", i), fmt.Sprintf("203.0.113.%d", i+1)) })
		}
		wg.Wait()
		counts := map[int]int{}
		for _, status := range statuses {
			counts[status]++
		}
		if counts[http.StatusUnauthorized] != 10 || counts[http.StatusTooManyRequests] != 20 {
			t.Errorf("30 guesses at once = %v; want exactly 10 checked (401), the rest 429", counts)
		}
	})

	a.logIn(priya) // another server's count, and her password is unchanged
}

// TestLinksAndLogins: a link works on the person's login (D64). It makes the login, or sets the
// password of the person's own login if no other business uses it. Any other login with the email,
// even one in no business, is only joined with its password. A forgotten password of a login also
// used in another business is never reset by a business's link.
func TestLinksAndLogins(t *testing.T) {
	a, b := newSite(t), newSite(t)
	inA, inB := "/t/"+a.slug, "/t/"+b.slug
	la, lb := a.logIn(lakshmi), b.logIn(lakshmi)

	t.Run("a login in no business yet is joined with its password, never reset", func(t *testing.T) {
		email := b.mail("meera")
		password, err := setup.NewPassword(context.Background(), b.app, email, "")
		if err != nil {
			t.Fatal(err)
		}
		b.passwords["meera"] = password
		code := b.addWithLink(lb, "Meera Pillai", email, "sales")
		if _, welcome, _ := b.get(b.browser(), inB+"/welcome/"+code); !strings.Contains(welcome, "Enter its password to add") ||
			strings.Contains(welcome, `name="next"`) {
			t.Errorf("the link for a login in no business: want join:\n%s", welcome)
		}
		m := b.browser()
		if status, _, _ := b.post(m, inB+"/welcome/"+code, url.Values{"mode": {"join"}, "password": {password}}, nil); status != http.StatusSeeOther {
			t.Fatalf("join = %d, want 303", status)
		}
		if status, home, _ := b.get(m, inB+"/"); status != http.StatusOK || !strings.Contains(home, "Meera Pillai") {
			t.Errorf("after the link, My Work = %d", status)
		}
		b.logIn("meera") // the operator's password, unchanged
	})

	t.Run("forgot password, in this business only: the link resets it", func(t *testing.T) {
		old := a.passwords[arjun]
		_, page, _ := a.post(la, inA+"/settings/people/"+a.people[arjun]+"/link", nil, nil)
		a.useLink(page, arjun, "arjun-new-pass-1")
		if status, _, _ := a.post(a.browser(), "/", url.Values{"email": {a.mail(arjun)}, "password": {old}}, nil); status != http.StatusUnauthorized {
			t.Errorf("the old password = %d, want refused", status)
		}
		a.logIn(arjun)
	})

	t.Run("a link for an email with no login, which then gets one elsewhere: open the link again", func(t *testing.T) {
		email := b.mail("kavya")
		code := b.addWithLink(lb, "Kavya Iyer", email, "sales") // no login yet: choose
		a.post(la, inA+"/settings/people", url.Values{"name": {"Kavya Iyer"}, "email": {email}, "role": {"sales"}}, nil)
		password, err := setup.NewPassword(context.Background(), a.app, email, a.tenant) // the operator, in a
		if err != nil {
			t.Fatal(err)
		}
		status, body, h := b.post(b.browser(), inB+"/welcome/"+code, url.Values{"mode": {"choose"}, "next": {"taken-over-pass-1"}, "again": {"taken-over-pass-1"}}, nil)
		body = html.UnescapeString(body)
		if status != http.StatusConflict || cookie(h, "seyal_account") != "" || !strings.Contains(body, "Please open the link again.") ||
			!strings.Contains(body, "Enter its password to add") {
			t.Errorf("using it as shown before = %d, want 409, no login, and the join form now:\n%s", status, body)
		}
		if status, _, _ := a.post(a.browser(), "/", url.Values{"email": {email}, "password": {password}}, nil); status != http.StatusSeeOther {
			t.Errorf("logging in with the login's password = %d; it should be unchanged", status)
		}
	})

	t.Run("forgot password, but the login is also in another business: refused", func(t *testing.T) {
		_, page, _ := a.post(la, inA+"/settings/people/"+a.people[arjun]+"/link", nil, nil)
		code := linkCode(t, page) // made while his login was in a only
		a.join(b, arjun, "sales")
		status, body, _ := a.get(a.browser(), inA+"/welcome/"+code)
		body = html.UnescapeString(body)
		if status != http.StatusOK || !strings.Contains(body, "Only the server's operator (whoever runs this Seyal server) can set") ||
			!strings.Contains(body, "Hello Arjun Mehta. This link can't set a new password.") || strings.Contains(body, "Welcome to") ||
			strings.Contains(body, `name="next"`) || strings.Contains(body, `name="password"`) {
			t.Errorf("the welcome page = %d, want the refusal without a password form:\n%s", status, body)
		}
		for _, form := range []url.Values{
			{"mode": {"choose"}, "next": {"arjun-third-pass"}, "again": {"arjun-third-pass"}},
			{"mode": {"join"}, "password": {a.passwords[arjun]}},
		} {
			if status, _, _ = a.post(a.browser(), inA+"/welcome/"+code, form, nil); status != http.StatusConflict {
				t.Errorf("using the link as %s = %d, want 409", form.Get("mode"), status)
			}
		}
		status, page, _ = a.post(la, inA+"/settings/people/"+a.people[arjun]+"/link", nil, nil)
		if status != http.StatusConflict || !strings.Contains(problem(page), "No link made for Arjun Mehta. Their Seyal login is also used for another business") ||
			shownLink.MatchString(page) {
			t.Errorf("making a new link now = %d, want 409, the reason and no link: %s", status, problem(page))
		}
		a.logIn(arjun)
	})

	a.tablesMatchLedger(t)
	b.tablesMatchLedger(t)
}

// TestEmailEditUnlinks: an admin who changes a linked person's email unlinks their login in that
// business only, and gets a new link for the new email at once. The login itself never changes, and
// a new email that already has a login is never linked on its own (D64).
func TestEmailEditUnlinks(t *testing.T) {
	a, b := newSite(t), newSite(t)
	inA, inB := "/t/"+a.slug+"/", "/t/"+b.slug+"/"
	lb := b.logIn(lakshmi)
	joined := a.join(b, arjun, "sales") // Arjun's login, in a and b
	arjunHere := a.logIn(arjun)
	if status, _, _ := b.get(arjunHere, inB); status != http.StatusOK {
		t.Fatalf("Arjun in b = %d", status)
	}
	victim := a.mail(lakshmi) // a's admin; her login is in a only
	la := a.logIn(lakshmi)

	status, page, _ := b.post(lb, inB+"settings/people/"+joined,
		url.Values{"name": {"Joined arjun"}, "email": {victim}, "role": {"sales"}, "status": {"active"}}, nil)
	page = html.UnescapeString(page)
	if status != http.StatusOK {
		t.Fatalf("edit = %d: %s", status, problem(page))
	}

	t.Run("Settings says so and offers a new link", func(t *testing.T) {
		want := "Saved. " + a.mail(arjun) + " will no longer reach Kaveri Associates (demo). Send the link below to Joined arjun's new email."
		if !strings.Contains(page, want) {
			t.Errorf("People page is missing %q:\n%s", want, page)
		}
		_, welcome, _ := b.get(b.browser(), inB+"welcome/"+linkCode(t, page))
		if !strings.Contains(html.UnescapeString(welcome), "You already have a Seyal login ("+victim+")") {
			t.Errorf("the new link should ask for the new email's own password:\n%s", welcome)
		}
	})

	t.Run("the old login loses b only", func(t *testing.T) {
		if status, _, _ := b.get(arjunHere, inB); status != http.StatusSeeOther {
			t.Errorf("Arjun in b after the edit = %d, want shut out", status)
		}
		if status, _, _ := a.get(arjunHere, inA); status != http.StatusOK {
			t.Errorf("Arjun in a = %d, want still in", status)
		}
		a.logIn(arjun) // his password is unchanged
	})

	t.Run("the victim isn't linked", func(t *testing.T) {
		if status, _, _ := b.get(la, inB); status != http.StatusSeeOther {
			t.Errorf("the new email's login in b = %d, want not a member", status)
		}
		if b.setUp(lb, victim) {
			t.Error("the person with the new email is shown as set up")
		}
		a.logIn(lakshmi)
	})

	t.Run("someone not set up yet: the link sent to the mistyped email stops, a new one is shown", func(t *testing.T) {
		typo := b.addWithLink(lb, "Devi Nair", b.mail("devi-typo"), "ops")
		status, page, _ := b.post(lb, inB+"settings/people/"+personWith(t, b, b.mail("devi-typo")),
			url.Values{"name": {"Devi Nair"}, "email": {b.mail("devi")}, "role": {"ops"}, "status": {"active"}}, nil)
		page = html.UnescapeString(page)
		if status != http.StatusOK || !strings.Contains(page, "Saved. Send the link below to Devi Nair's new email.") ||
			strings.Contains(page, "will no longer reach") {
			t.Fatalf("edit = %d: %s", status, page)
		}
		if status, _, _ := b.get(b.browser(), inB+"welcome/"+typo); status != http.StatusNotFound {
			t.Errorf("the link sent to the mistyped email = %d, want 404", status)
		}
		if _, welcome, _ := b.get(b.browser(), inB+"welcome/"+linkCode(t, page)); !strings.Contains(welcome, "Choose your password") ||
			!strings.Contains(welcome, b.mail("devi")) {
			t.Errorf("the new link should set up the new email:\n%s", welcome)
		}
	})

	t.Run("an admin can't change their own email", func(t *testing.T) {
		if _, page, _ := b.get(lb, inB+"settings/people"); !strings.Contains(page, "Another admin can change your login email.") ||
			!strings.Contains(page, `value="`+b.mail(lakshmi)+`" required readonly`) {
			t.Errorf("own row in Settings should keep the email read-only:\n%s", page)
		}
		status, page, _ := b.post(lb, inB+"settings/people/"+b.people[lakshmi],
			url.Values{"name": {"Lakshmi Narayanan"}, "email": {b.mail("lakshmi-new")}, "role": {"admin", "owner"}, "status": {"active"}}, nil)
		if status != http.StatusUnprocessableEntity || !strings.Contains(problem(page), "You can't change your own login email") {
			t.Errorf("own email change = %d: %s", status, problem(page))
		}
		// U+017F (long s) counts as "s" for strings.EqualFold, but it is saved as another address.
		long := strings.Replace(b.mail(lakshmi), "s", "\u017f", 1)
		status, page, _ = b.post(lb, inB+"settings/people/"+b.people[lakshmi],
			url.Values{"name": {"Lakshmi Narayanan"}, "email": {long}, "role": {"admin", "owner"}, "status": {"active"}}, nil)
		if status != http.StatusUnprocessableEntity || !strings.Contains(problem(page), "You can't change your own login email") {
			t.Errorf("own email changed to %q = %d: %s", long, status, problem(page))
		}
		// Only the letter case: the same address, saved as it was.
		status, page, _ = b.post(lb, inB+"settings/people/"+b.people[lakshmi],
			url.Values{"name": {"Lakshmi Narayanan"}, "email": {" " + strings.ToUpper(b.mail(lakshmi))}, "role": {"admin", "owner"}, "status": {"active"}}, nil)
		if status != http.StatusOK || strings.Contains(page, "will no longer reach") {
			t.Errorf("own email in capitals = %d: %s", status, problem(page))
		}
		if status, _, _ := b.get(lb, inB); status != http.StatusOK {
			t.Errorf("still logged in after saving = %d", status)
		}
	})

	a.tablesMatchLedger(t)
	b.tablesMatchLedger(t)
}

// TestSeyalLink: the operator adds an existing login to a business; the password stays.
func TestSeyalLink(t *testing.T) {
	ctx := context.Background()
	a, b := newSite(t), newSite(t)
	email := a.mail(arjun)
	c := a.logIn(arjun)
	b.addWithLink(b.logIn(lakshmi), "Arjun Mehta", email, "sales") // the link isn't used

	if linked, err := setup.Link(ctx, b.app, strings.ToUpper(email), b.tenant); err != nil || !linked {
		t.Fatalf("link = %v, %v", linked, err)
	}
	if status, _, _ := b.get(c, "/t/"+b.slug+"/"); status != http.StatusOK {
		t.Errorf("b with the same cookie = %d", status)
	}
	a.logIn(arjun) // the password is unchanged
	if linked, err := setup.Link(ctx, b.app, email, b.tenant); err != nil || linked {
		t.Errorf("linking again = %v, %v; want nothing changed", linked, err)
	}
	if _, err := setup.Link(ctx, b.app, b.mail("nobody"), b.tenant); err == nil || !strings.Contains(err.Error(), "no login with the email") {
		t.Errorf("an email without a login = %v", err)
	}
	if _, err := setup.Link(ctx, b.app, a.mail(priya), b.tenant); err == nil || !strings.Contains(err.Error(), "nobody has the login email") {
		t.Errorf("a login whose email isn't in the business = %v", err)
	}
	var how string
	if err := db.WithTenant(ctx, b.app, b.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT body FROM ledger WHERE kind = 'person.account_linked' ORDER BY seq DESC LIMIT 1").Scan(&how)
	}); err != nil || !strings.Contains(how, `"how":"operator"`) {
		t.Errorf("the event = %s, %v", how, err)
	}
	a.tablesMatchLedger(t)
	b.tablesMatchLedger(t)
}
