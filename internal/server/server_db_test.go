//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/ai"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/server"
	"seyal/internal/setup"
	"seyal/internal/testdb"
)

// site is the web app running against a fresh copy of the demo business.
type site struct {
	t         *testing.T
	srv       *httptest.Server
	slug      string
	tenant    string
	store     media.Disk
	owner     *pgxpool.Pool // for tests that must change what the app may not (e.g. age a link)
	app       *pgxpool.Pool
	passwords map[string]string // demo key (or a made-up name like vikram) → password
	people    map[string]string // demo key → person id
	ai        *ai.Setup         // AI is off unless a test sets a (fake) model
	lib       *playbook.Library
}

// The demo people, by their key in the demo file. Each site gives them their own emails (s.mail),
// because a login email is unique across the whole server and the test sites share one database.
const (
	arjun   = "arjun"   // sales
	priya   = "priya"   // ops
	lakshmi = "lakshmi" // owner and admin
)

func newSite(t *testing.T) *site {
	t.Helper()
	return newSiteWith(t, "../testdata/playbooks")
}

// newSiteWith is newSite with the processes from another folder, e.g. the shipped samples.
func newSiteWith(t *testing.T, playbooks string) *site {
	t.Helper()
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	lib, err := playbook.LoadLibrary(playbooks, "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := setup.Load("../../examples/seed/demo.yaml", lib)
	if err != nil {
		t.Fatal(err)
	}
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10])
	s := &site{t: t, slug: f.Business.Slug, store: media.Disk{Dir: t.TempDir()}, owner: owner, app: app,
		passwords: map[string]string{}, ai: &ai.Setup{USDtoINR: 88}, lib: lib}
	for i, p := range f.People {
		f.People[i].Email = s.mail(p.Key)
	}
	sum, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	s.tenant, s.people = sum.TenantID, sum.People
	for _, key := range []string{arjun, priya, lakshmi} {
		if s.passwords[key], err = setup.NewPassword(ctx, app, s.mail(key), sum.TenantID); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := server.New(server.Config{App: app, Store: s.store, Library: lib, AI: s.ai})
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(s.srv.Close)
	return s
}

// mail is the email of name (a demo key like arjun, or a made-up person like vikram) on this site
// only: name@<slug>.kaveri-demo.example.
func (s *site) mail(name string) string {
	return name + "@" + s.slug + ".kaveri-demo.example"
}

// browser keeps cookies and doesn't follow redirects, so tests can see them.
func (s *site) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (s *site) get(c *http.Client, path string) (int, string, http.Header) {
	s.t.Helper()
	resp, err := c.Get(s.srv.URL + path)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func (s *site) post(c *http.Client, path string, form url.Values, header map[string]string) (int, string, http.Header) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// logIn logs the person called name in with their password (see s.passwords).
func (s *site) logIn(name string) *http.Client {
	s.t.Helper()
	c := s.browser()
	code, _, h := s.post(c, "/t/"+s.slug+"/login", url.Values{"email": {s.mail(name)}, "password": {s.passwords[name]}}, nil)
	if code != http.StatusSeeOther || h.Get("Location") != "/t/"+s.slug+"/" {
		s.t.Fatalf("login as %s = %d to %q", name, code, h.Get("Location"))
	}
	return c
}

var shownLink = regexp.MustCompile(`/welcome/([A-Za-z0-9_-]{43})`)

// useLink opens the set-your-password link shown on a page, chooses password, and returns the
// browser, now logged in as name.
func (s *site) useLink(page, name, password string) *http.Client {
	s.t.Helper()
	m := shownLink.FindStringSubmatch(page)
	if m == nil {
		s.t.Fatalf("no link on the page:\n%s", problem(page))
	}
	c := s.browser()
	code, body, h := s.post(c, "/t/"+s.slug+"/welcome/"+m[1], url.Values{"next": {password}, "again": {password}}, nil)
	if code != http.StatusSeeOther || h.Get("Location") != "/t/"+s.slug+"/" {
		s.t.Fatalf("using the link = %d to %q:\n%s", code, h.Get("Location"), body)
	}
	s.passwords[name] = password
	return c
}

// temporaryPassword gives someone's login a temporary password, as an admin could make before
// links (logins on running servers may still have one), and returns it.
func (s *site) temporaryPassword(name string) string {
	s.t.Helper()
	password, err := setup.NewPassword(context.Background(), s.app, s.mail(name), "")
	if err != nil {
		s.t.Fatal(err)
	}
	// The app may no longer make one; the owner marks it.
	if _, err := s.owner.Exec(context.Background(), "UPDATE accounts SET password_temporary = true WHERE email = $1", s.mail(name)); err != nil {
		s.t.Fatal(err)
	}
	s.passwords[name] = password
	return password
}

// send posts a form and expects to be sent back to the item, then returns the item page.
func (s *site) send(c *http.Client, path string, form url.Values) string {
	s.t.Helper()
	code, body, h := s.post(c, path, form, nil)
	if code != http.StatusSeeOther {
		s.t.Fatalf("POST %s %v = %d:\n%s", path, form, code, problem(body))
	}
	_, page, _ := s.get(c, h.Get("Location"))
	return page
}

var banner = regexp.MustCompile(`(?s)<p class="banner error" role="alert">(.*?)</p>`)

// problem returns the problem shown on a page, as people read it (HTML entities decoded).
func problem(body string) string {
	if m := banner.FindStringSubmatch(body); m != nil {
		return html.UnescapeString(m[1])
	}
	return "(no problem shown)"
}

func TestLoginPage(t *testing.T) {
	s := newSite(t)
	home := "/t/" + s.slug + "/"

	t.Run("pages without a login send you to the login page", func(t *testing.T) {
		code, _, h := s.get(s.browser(), home)
		if code != http.StatusSeeOther || h.Get("Location") != home+"login" {
			t.Errorf("home without login = %d to %q", code, h.Get("Location"))
		}
	})

	t.Run("the login page doesn't reveal the business", func(t *testing.T) {
		_, real, h := s.get(s.browser(), home+"login")
		_, fake, _ := s.get(s.browser(), "/t/no-such-firm/login")
		_, root, _ := s.get(s.browser(), "/")
		if strings.Contains(real, "Kaveri") {
			t.Error("the login page shows the business's name")
		}
		// The page contains the address you typed (for the app manifest); apart from that it
		// must be identical, so it can't tell real addresses from made-up ones. "/" is the same form
		// without a manifest.
		if strings.ReplaceAll(fake, "no-such-firm", s.slug) != real {
			t.Error("a real address and a made-up one give different login pages")
		}
		if strings.Replace(real, `<link rel="manifest" href="`+home+`manifest.webmanifest">`, "", 1) != root {
			t.Error(`"/" doesn't show the same login form`)
		}
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "script-src 'self'") || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("security headers = %v", h)
		}
	})

	t.Run("a made-up address answers like a wrong password", func(t *testing.T) {
		code, body, _ := s.post(s.browser(), "/t/no-such-firm/login", url.Values{"email": {s.mail(arjun)}, "password": {"x"}}, nil)
		if code != http.StatusUnauthorized || !strings.Contains(body, "Wrong email or password") {
			t.Errorf("made-up address = %d", code)
		}
		_, unknown, _ := s.post(s.browser(), home+"login", url.Values{"email": {s.mail(arjun)}, "password": {"x"}}, nil)
		if strings.ReplaceAll(body, "no-such-firm", s.slug) != unknown {
			t.Error("a wrong password answers differently on a made-up address")
		}
		if code, _, h := s.get(s.browser(), "/t/no-such-firm/"); code != http.StatusSeeOther ||
			h.Get("Location") != "/t/no-such-firm/login" {
			t.Errorf("made-up address home = %d to %q, want the same as a real one", code, h.Get("Location"))
		}
	})

	t.Run("the right password on an address the login isn't in goes to /, like a made-up one", func(t *testing.T) {
		other := newSite(t) // its people have their own logins, in that business only
		them := url.Values{"email": {other.mail(arjun)}, "password": {other.passwords[arjun]}}
		code, body, h := s.post(s.browser(), home+"login", them, nil)
		fcode, fbody, fh := s.post(s.browser(), "/t/no-such-firm/login", them, nil)
		if code != http.StatusSeeOther || h.Get("Location") != "/" || fcode != code || fh.Get("Location") != h.Get("Location") || fbody != body {
			t.Errorf("not a member = %d to %q, made-up = %d to %q; want the same 303 to /", code, h.Get("Location"), fcode, fh.Get("Location"))
		}
	})

	t.Run("one login cookie for the whole server", func(t *testing.T) {
		_, _, h := s.post(s.browser(), home+"login", url.Values{"email": {s.mail(arjun)}, "password": {s.passwords[arjun]}}, nil)
		cookie := h.Get("Set-Cookie")
		if !strings.HasPrefix(cookie, "seyal_account=") || !strings.Contains(cookie, "Path=/;") ||
			!strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Lax") {
			t.Errorf("cookie = %q", cookie)
		}
	})

	t.Run("another site can't post to Seyal", func(t *testing.T) {
		c := s.logIn(arjun)
		cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
		for _, path := range []string{"/", "/logout", home + "login", home + "logout", home + "logout-everywhere"} {
			form := url.Values{"email": {s.mail(arjun)}, "password": {s.passwords[arjun]}}
			if code, _, _ := s.post(c, path, form, cross); code != http.StatusForbidden {
				t.Errorf("cross-site POST %s = %d, want 403", path, code)
			}
		}
		if code, _, _ := s.get(c, home); code != http.StatusOK {
			t.Errorf("after the refused posts, home = %d; want still logged in", code)
		}
	})

	t.Run("logout ends the session", func(t *testing.T) {
		c := s.logIn(arjun)
		code, _, h := s.post(c, home+"logout", nil, nil)
		if code != http.StatusSeeOther || h.Get("Location") != home+"login" {
			t.Fatalf("logout = %d to %q", code, h.Get("Location"))
		}
		if cookies := strings.Join(h.Values("Set-Cookie"), "\n"); !strings.Contains(cookies, "seyal_account=; Path=/; Max-Age=0") ||
			!strings.Contains(cookies, "seyal_session=; Path="+home+"; Max-Age=0") {
			t.Errorf("logout cookies = %s; want the login cookie and this address's old one removed", cookies)
		}
		if code, _, _ := s.get(c, home); code != http.StatusSeeOther {
			t.Errorf("home after logout = %d, want a redirect", code)
		}
	})

	t.Run("pages link static files by fingerprint, so browsers never keep an old copy", func(t *testing.T) {
		_, body, _ := s.get(s.browser(), home+"login")
		if !regexp.MustCompile(`href="/static/app\.css\?v=[0-9a-f]{12}"`).MatchString(body) {
			t.Error("app.css is linked without a fingerprint")
		}
	})

	t.Run("static files", func(t *testing.T) {
		for _, path := range []string{"/static/htmx.min.js", "/static/app.css", "/static/fonts.css"} {
			if code, _, h := s.get(s.browser(), path); code != http.StatusOK || h.Get("Cache-Control") == "" {
				t.Errorf("%s = %d", path, code)
			}
		}
	})

	t.Run("too many wrong passwords are blocked, on every address and on / together", func(t *testing.T) {
		// Behind a proxy, each attempt can come from another visitor's address, so only the count
		// per email blocks them.
		srv, err := server.New(server.Config{App: s.app, Store: s.store, Library: s.lib, AI: s.ai, BehindProxy: true})
		if err != nil {
			t.Fatal(err)
		}
		proxied := httptest.NewServer(srv.Handler())
		defer proxied.Close()
		paths := []string{home + "login", "/", "/t/no-such-firm/login", "/t/test-" + strings.ToLower(rand.Text()[:10]) + "/login"}
		attempt := func(i int, password string) int {
			form := url.Values{"email": {s.mail(lakshmi)}, "password": {password}}
			req, _ := http.NewRequest(http.MethodPost, proxied.URL+paths[i%len(paths)], strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i+1))
			resp, err := s.browser().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		for i := 0; i < 10; i++ {
			if code := attempt(i, "guess "+string(rune('a'+i))); code != http.StatusUnauthorized {
				t.Fatalf("wrong password %d = %d, want 401", i+1, code)
			}
		}
		if code := attempt(10, s.passwords[lakshmi]); code != http.StatusTooManyRequests {
			t.Errorf("after 10 wrong passwords spread over addresses, the right one = %d, want 429", code)
		}
	})

	t.Run("too many wrong passwords from one visitor are blocked, whatever the email", func(t *testing.T) {
		blocked := false
		for i := 0; i < 12 && !blocked; i++ {
			path := []string{"/", "/t/no-such-firm/login"}[i%2]
			code, _, _ := s.post(s.browser(), path, url.Values{"email": {s.mail(fmt.Sprint("nobody", i))}, "password": {"guess"}}, nil)
			blocked = code == http.StatusTooManyRequests
		}
		if !blocked {
			t.Error("12 wrong logins from one visitor were never blocked")
		}
	})
}

// TestAJobInTheBrowser walks a whole job through the screens, as people would.
func TestAJobInTheBrowser(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)

	_, home, _ := s.get(a, base+"/")
	for _, want := range []string{"My Work", "Tell Seyal", "Arjun Mehta", "Kaveri Associates (demo)", "What you can start",
		"Job evaluation", "Evaluate a new piece of client work", `placeholder="For example: New monthly bookkeeping job for Acme Traders`} {
		if !strings.Contains(home, want) {
			t.Fatalf("home page is missing %q", want)
		}
	}
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
	if pv == nil {
		t.Fatal("no playbook to tell Seyal about")
	}

	code, hello, _ := s.post(a, base+"/tell", url.Values{"playbook": {pv[1]}, "text": {"Hello"}}, nil)
	if code != http.StatusOK || !strings.Contains(hello, "Tell me about the work you want to start") {
		t.Fatalf("a greeting on Tell Seyal = %d; want the screen again with Seyal's reply", code)
	}

	code, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv[1]}, "text": {"New monthly books job for Sundaram"}}, nil)
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/items/") {
		t.Fatalf("tell = %d to %q", code, h.Get("Location"))
	}
	item := h.Get("Location")
	_, page, _ := s.get(a, item)
	if !strings.Contains(page, "Which client is this for?") || !strings.Contains(page, ">Sundaram Textiles</button>") {
		t.Fatalf("the item page doesn't ask for the client with quick answers:\n%s", page)
	}

	say := func(text string) string { return s.send(a, item+"/say", url.Values{"text": {text}}) }
	say("Sundaram Textiles")
	say("Monthly bookkeeping")
	say("Books for the year")
	page = say("10k")
	if !strings.Contains(page, "₹12,000") || !strings.Contains(page, "₹10,000") || !strings.Contains(page, "Karthik Subramanian") {
		t.Errorf("fields don't show the standard fee, agreed fee and pre-filled maker")
	}
	page = say("pass")
	if !strings.Contains(page, "Everything needed is filled in") {
		t.Errorf("Seyal didn't say everything is filled in")
	}

	page = s.send(a, item+"/act", url.Values{"action": {"submit"}})
	if !strings.Contains(page, "pending approval") {
		t.Fatal("not pending approval after submit")
	}
	// The fee is below standard, so the owner must approve too; the row says why.
	if !strings.Contains(page, "Owner") || !strings.Contains(page, "required by the playbook&#39;s rule") {
		t.Errorf("owner approval and its reason are not shown")
	}

	t.Run("Arjun approves as sales", func(t *testing.T) {
		page := s.send(a, item+"/decide", url.Values{"slot": {"sales"}, "decision": {"approve"}})
		if !strings.Contains(page, "✓ Arjun Mehta") {
			t.Error("sales approval not shown")
		}
	})

	t.Run("Priya sees it waiting for her and approves as ops", func(t *testing.T) {
		p := s.logIn(priya)
		_, home, _ := s.get(p, base+"/")
		waiting := home[strings.Index(home, "Needs my approval"):]
		if !strings.Contains(waiting, "Sundaram Textiles") {
			t.Error("the job isn't in Priya's 'Needs my approval'")
		}
		page := s.send(p, item+"/decide", url.Values{"slot": {"ops"}, "decision": {"approve"}})
		if !strings.Contains(page, "✓ Priya Raman") || !strings.Contains(page, "pending approval") {
			t.Error("after ops: still waiting for the owner")
		}
	})

	t.Run("refusals are explained, not crashes", func(t *testing.T) {
		code, body, _ := s.post(a, item+"/decide", url.Values{"slot": {"owner"}, "decision": {"approve"}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "role") {
			t.Errorf("Arjun approving as owner = %d: %s", code, problem(body))
		}
		code, body, _ = s.post(a, item+"/act", url.Values{"action": {"cancel"}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "reason") {
			t.Errorf("cancel without a reason = %d: %s", code, problem(body))
		}
	})

	t.Run("a voice note in the thread can be played back", func(t *testing.T) {
		voice := []byte("pretend opus audio")
		id := item[strings.LastIndex(item, "/")+1:]
		err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
			_, _, err := channel.Receive(context.Background(), tx, s.store, channel.Incoming{Channel: "web",
				PersonID: s.people["arjun"], ThreadID: id, Files: []channel.File{{ContentType: "audio/webm", Data: voice}}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		_, page, _ := s.get(a, item)
		src := regexp.MustCompile(`<audio controls preload="none" src="([^"]+)"`).FindStringSubmatch(page)
		if src == nil {
			t.Fatal("no audio player in the thread")
		}
		code, body, h := s.get(a, src[1])
		if code != http.StatusOK || body != string(voice) || h.Get("Content-Type") != "audio/webm" {
			t.Errorf("voice note = %d %q %q", code, h.Get("Content-Type"), body)
		}
		if code, _, _ := s.get(s.browser(), src[1]); code != http.StatusSeeOther {
			t.Errorf("voice note without login = %d, want a redirect to login", code)
		}
	})
}

// upload posts a composer form the way the browser does when a voice note or file is attached.
func (s *site) upload(c *http.Client, path string, fields map[string]string, files []upFile) (int, string, http.Header) {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+f.field+`"; filename="`+f.name+`"`)
		h.Set("Content-Type", f.contentType)
		part, _ := mw.CreatePart(h)
		part.Write(f.data)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

type upFile struct {
	field, name, contentType string
	data                     []byte
}

func TestVoiceNotesAndAttachments(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	voice := upFile{"voice", "voice-note.webm", "audio/webm;codecs=opus", []byte("pretend opus recording")}

	t.Run("a job can start from a voice note alone", func(t *testing.T) {
		code, body, h := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "text": ""}, []upFile{voice})
		if code != http.StatusSeeOther {
			t.Fatalf("tell with voice = %d: %s", code, problem(body))
		}
		_, page, _ := s.get(a, h.Get("Location"))
		if !strings.Contains(page, "🎙 Voice note") || !strings.Contains(page, "<audio controls") ||
			!strings.Contains(page, "Which client is this for?") {
			t.Error("the new job doesn't show the voice note and Seyal's first question")
		}
	})

	t.Run("voice note and photo in a thread", func(t *testing.T) {
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"New job"}}, nil)
		item := h.Get("Location")
		photo := upFile{"file", "statement.jpg", "image/jpeg", []byte("\xff\xd8\xff pretend jpeg")}
		code, body, _ := s.upload(a, item+"/say", map[string]string{"text": "Client sent this"}, []upFile{voice, photo})
		if code != http.StatusSeeOther {
			t.Fatalf("say with files = %d: %s", code, problem(body))
		}
		_, page, _ := s.get(a, item)
		if strings.Count(page, "/media/") < 2 || !strings.Contains(page, "Client sent this") {
			t.Error("the thread doesn't show the text, the voice note and the photo")
		}
	})

	t.Run("a web page disguised as an attachment is refused", func(t *testing.T) {
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"New job"}}, nil)
		bad := upFile{"file", "x.html", "text/html", []byte("<script>alert(1)</script>")}
		code, body, _ := s.upload(a, h.Get("Location")+"/say", map[string]string{"text": ""}, []upFile{bad})
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "not accepted") {
			t.Errorf("html attachment = %d: %s", code, problem(body))
		}
	})

	t.Run("an empty message is refused", func(t *testing.T) {
		code, body, _ := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "text": "  "}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "record") {
			t.Errorf("empty tell = %d: %s", code, problem(body))
		}
	})
}

func TestInstallableApp(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug + "/"

	t.Run("pages point at the manifest and icons", func(t *testing.T) {
		_, page, _ := s.get(s.browser(), base+"login")
		for _, want := range []string{`rel="manifest" href="` + base + `manifest.webmanifest"`,
			`name="theme-color"`, `rel="apple-touch-icon"`} {
			if !strings.Contains(page, want) {
				t.Errorf("login page is missing %s", want)
			}
		}
	})

	t.Run("manifest opens the business's own address, without naming it", func(t *testing.T) {
		code, body, h := s.get(s.browser(), base+"manifest.webmanifest")
		var m struct {
			Name, StartURL, Scope, Display string
			Icons                          []struct{ Src, Sizes, Purpose string }
		}
		if err := json.Unmarshal([]byte(strings.NewReplacer(`"start_url"`, `"StartURL"`).Replace(body)), &m); err != nil {
			t.Fatal(err)
		}
		if code != http.StatusOK || h.Get("Content-Type") != "application/manifest+json" || m.Name != "Seyal" ||
			m.StartURL != base || m.Scope != base || m.Display != "standalone" || strings.Contains(body, "Kaveri") {
			t.Errorf("manifest = %d %s", code, body)
		}
		for _, icon := range m.Icons {
			if code, _, _ := s.get(s.browser(), icon.Src); code != http.StatusOK {
				t.Errorf("icon %s = %d", icon.Src, code)
			}
		}
		_, fake, _ := s.get(s.browser(), "/t/no-such-firm/manifest.webmanifest")
		if strings.ReplaceAll(fake, "no-such-firm", s.slug) != body {
			t.Error("a made-up address gets a different manifest")
		}
	})

	t.Run("service worker and offline page", func(t *testing.T) {
		code, body, h := s.get(s.browser(), "/sw.js")
		if code != http.StatusOK || h.Get("Cache-Control") != "no-cache" || strings.Contains(body, "__VERSION__") ||
			!regexp.MustCompile(`const VERSION = "[0-9a-f]{12}"`).MatchString(body) {
			t.Errorf("sw.js = %d, cache %q", code, h.Get("Cache-Control"))
		}
		if code, page, _ := s.get(s.browser(), "/offline"); code != http.StatusOK || !strings.Contains(page, "You're offline") {
			t.Errorf("offline page = %d", code)
		}
	})
}

func TestEditFieldsFromSidePanel(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"GST work for Sundaram"}}, nil)
	item := h.Get("Location")
	edit := func(key string, values ...string) string {
		t.Helper()
		return s.send(a, item+"/field", url.Values{"key": {key}, "value": values})
	}

	page := edit("nature_of_work", "gst-returns")
	if !strings.Contains(page, "Fatima Sheikh") || !strings.Contains(page, "from Nature of work") {
		t.Fatal("choosing the work type should pre-fill the maker and mark it as a suggestion")
	}

	t.Run("pre-filled maker can be changed for this job", func(t *testing.T) {
		page := edit("maker", s.people["rahul"])
		maker := page[strings.Index(page, "<dt>Maker</dt>"):]
		maker = maker[:strings.Index(maker, "</dd>")]
		if !strings.Contains(maker, "Rahul Iyer") || strings.Contains(maker, "from Nature of work") {
			t.Errorf("maker after the change = %s", maker)
		}
	})

	t.Run("documents, one per line", func(t *testing.T) {
		page := edit("required_documents", "Sales register\nBank statement\n")
		if !strings.Contains(page, "Sales register, Bank statement") {
			t.Error("documents list not saved")
		}
	})

	t.Run("fee in everyday words", func(t *testing.T) {
		if page := edit("fee_agreed", "2000 per month"); !strings.Contains(page, "₹2,000") {
			t.Error("fee not understood")
		}
	})

	t.Run("bad values and read-only fields are refused", func(t *testing.T) {
		code, body, _ := s.post(a, item+"/field", url.Values{"key": {"fee_agreed"}, "value": {"lots"}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "Fee agreed") {
			t.Errorf("bad fee = %d: %s", code, problem(body))
		}
		code, body, _ = s.post(a, item+"/field", url.Values{"key": {"standard_fee"}, "value": {"1"}}, nil)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("editing the standard fee = %d: %s", code, problem(body))
		}
	})

	t.Run("an edit after approval resets the approvals", func(t *testing.T) {
		edit("client", "sundaram-textiles")
		edit("reason", "Monthly returns")
		edit("integrity_check", "pass")
		edit("fee_agreed", "5000")
		s.send(a, item+"/act", url.Values{"action": {"submit"}})
		s.send(a, item+"/decide", url.Values{"slot": {"sales"}, "decision": {"approve"}})
		page := edit("advance", "1000")
		if strings.Contains(page, "✓ Arjun Mehta") || !strings.Contains(page, "pending approval") {
			t.Error("the sales approval still counts after the advance changed")
		}
	})
}

func testdbApp(t *testing.T) *pgxpool.Pool {
	_, app := testdb.Connect(t)
	return app
}
