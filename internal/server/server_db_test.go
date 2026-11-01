//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
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
	passwords map[string]string // email → password
	people    map[string]string // demo key → person id
}

const (
	arjun = "arjun@kaveri-demo.example" // sales
	priya = "priya@kaveri-demo.example" // ops
)

func newSite(t *testing.T) *site {
	t.Helper()
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := setup.Load("../../examples/seed/demo.yaml", lib)
	if err != nil {
		t.Fatal(err)
	}
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10])
	sum, err := setup.Import(ctx, owner, app, lib, f)
	if err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, slug: f.Business.Slug, tenant: sum.TenantID, store: media.Disk{Dir: t.TempDir()},
		passwords: map[string]string{}, people: sum.People}
	for _, email := range []string{arjun, priya} {
		if s.passwords[email], err = setup.NewPassword(ctx, app, sum.TenantID, email); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := server.New(app, s.store, false)
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(s.srv.Close)
	return s
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

func (s *site) logIn(email string) *http.Client {
	s.t.Helper()
	c := s.browser()
	code, _, h := s.post(c, "/t/"+s.slug+"/login", url.Values{"email": {email}, "password": {s.passwords[email]}}, nil)
	if code != http.StatusSeeOther || h.Get("Location") != "/t/"+s.slug+"/" {
		s.t.Fatalf("login as %s = %d to %q", email, code, h.Get("Location"))
	}
	return c
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

func problem(body string) string {
	if m := banner.FindStringSubmatch(body); m != nil {
		return m[1]
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
		if strings.Contains(real, "Kaveri") {
			t.Error("the login page shows the business's name")
		}
		// The page contains the address you typed (for the app manifest); apart from that it
		// must be identical, so it can't tell real addresses from made-up ones.
		if strings.ReplaceAll(fake, "no-such-firm", s.slug) != real {
			t.Error("a real address and a made-up one give different login pages")
		}
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "script-src 'self'") || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("security headers = %v", h)
		}
	})

	t.Run("a made-up address answers like a wrong password", func(t *testing.T) {
		code, body, _ := s.post(s.browser(), "/t/no-such-firm/login", url.Values{"email": {arjun}, "password": {"x"}}, nil)
		if code != http.StatusUnauthorized || !strings.Contains(body, "Wrong email or password") {
			t.Errorf("made-up address = %d", code)
		}
		if code, _, h := s.get(s.browser(), "/t/no-such-firm/"); code != http.StatusSeeOther ||
			h.Get("Location") != "/t/no-such-firm/login" {
			t.Errorf("made-up address home = %d to %q, want the same as a real one", code, h.Get("Location"))
		}
	})

	t.Run("cookie is HttpOnly and SameSite", func(t *testing.T) {
		_, _, h := s.post(s.browser(), home+"login", url.Values{"email": {arjun}, "password": {s.passwords[arjun]}}, nil)
		if cookie := h.Get("Set-Cookie"); !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Lax") {
			t.Errorf("cookie = %q", cookie)
		}
	})

	t.Run("another site can't post to Seyal", func(t *testing.T) {
		c := s.logIn(arjun)
		if code, _, _ := s.post(c, home+"logout", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
			t.Errorf("cross-site logout = %d, want 403", code)
		}
	})

	t.Run("logout ends the session", func(t *testing.T) {
		c := s.logIn(arjun)
		if code, _, h := s.post(c, home+"logout", nil, nil); code != http.StatusSeeOther || h.Get("Location") != home+"login" {
			t.Fatalf("logout = %d to %q", code, h.Get("Location"))
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

	t.Run("too many wrong passwords are blocked", func(t *testing.T) {
		blocked := false
		for i := 0; i < 12 && !blocked; i++ {
			code, _, _ := s.post(s.browser(), home+"login",
				url.Values{"email": {arjun}, "password": {"guess " + string(rune('a'+i))}}, nil)
			blocked = code == http.StatusTooManyRequests
		}
		if !blocked {
			t.Error("12 wrong passwords in a row were never blocked")
		}
	})
}

// TestAJobInTheBrowser walks a whole job through the screens, as people would.
func TestAJobInTheBrowser(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)

	_, home, _ := s.get(a, base+"/")
	for _, want := range []string{"My Work", "Tell Seyal", "Arjun Mehta", "Kaveri Associates (demo)", "Fee agreed"} {
		if !strings.Contains(home, want) {
			t.Fatalf("home page is missing %q", want)
		}
	}
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
	if pv == nil {
		t.Fatal("no playbook to tell Seyal about")
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
		waiting := home[strings.Index(home, "Waiting for my approval"):]
		if !strings.Contains(waiting, "Sundaram Textiles") {
			t.Error("the job isn't in Priya's 'Waiting for my approval'")
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

func testdbApp(t *testing.T) *pgxpool.Pool {
	_, app := testdb.Connect(t)
	return app
}
