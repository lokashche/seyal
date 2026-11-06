// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server is Seyal's web app: one working screen with three panes (My Work, the thread,
// fields & approvals), following docs/seyal-concept.html, plus login. Each business has its own
// address, /t/<slug>/.
package server

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/ai"
	"seyal/internal/auth"
	"seyal/internal/chase"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/seal"
	"seyal/web"
)

// Cookies. One login cookie for the whole server (D64); each page still checks, inside its business,
// that the login reaches an active person there.
const (
	accountCookie = "seyal_account" // the login session's token, for every address (Path "/")
	lastCookie    = "seyal_last"    // the business last used on this device, where "/" goes next time
	// legacyCookie was each business address's own login cookie (Path /t/<slug>/), before one login
	// across businesses. For one release it is swapped for the new one (auth.Upgrade); remove with it.
	legacyCookie = "seyal_session"
)

// Server serves the web app.
type Server struct {
	app     *pgxpool.Pool
	store   media.Store
	lib     *playbook.Library // templates: sample processes and lists a business can start from
	ai      *ai.Setup
	secure  bool // send cookies only over HTTPS
	proxied bool // behind a trusted reverse proxy that sets X-Forwarded-For
	pages   map[string]*template.Template
	static  http.Handler
	asset   string // fingerprint of the static files, added to their addresses
	worker  []byte // the service worker script, with the fingerprint filled in
	limiter *limiter
	// speaking, voiceRate and audioUsed limit the calls to the speech service (voice.go).
	speaking  *speechSlots
	voiceRate *limiter
	audioUsed *audioUse
	// voiceKey signs what the speech service heard, for the message the person sends (voice.go). It
	// is made anew at every start, so it is never stored anywhere.
	voiceKey []byte
	// askRate limits the questions about the work each login may ask (app.go).
	askRate *limiter
	// limit is how long the web server gives one request (RequestLimit unless a test sets another).
	limit time.Duration
	// pushKey is the server's public push key for phone notes (Config.PushKey); "" means they are off.
	pushKey string
	// turnOnRate and helloRate limit the phone-note routes each login may call (phonenotes.go).
	turnOnRate, helloRate *limiter
}

// RequestLimit is how long the web server gives one request to be read and answered: cmd/seyal sets
// http.Server's ReadTimeout and WriteTimeout to it. After that the person's connection is dropped,
// so slow work (a question's second AI call, app.go) checks the time left first.
const RequestLimit = 2 * time.Minute

// Config is what the web app needs.
type Config struct {
	App           *pgxpool.Pool     // the app role's connections
	Store         media.Store       // voice notes and files
	Library       *playbook.Library // templates a business can start from (its own are in the database)
	AI            *ai.Setup         // models per purpose; nil means AI is off
	SecureCookies bool              // true when the app is reached over HTTPS
	// BehindProxy says a reverse proxy we run (Caddy) is the only way in, so the visitor's address is
	// the last one it adds to X-Forwarded-For. Never set it when people can reach the app directly.
	BehindProxy bool
	// RequestLimit is the web server's time limit for one request; zero means RequestLimit. Only
	// tests set another, to show what happens when little time is left.
	RequestLimit time.Duration
	// PushKey is the server's public push key (SEYAL_PUSH_PUBLIC_KEY) when phone notes are switched
	// on (SEYAL_PUSH=on). Empty means they are off: Me offers no Turn on, so no browser asks its
	// push service for an address.
	PushKey string
}

// New prepares the web app.
func New(cfg Config) (*Server, error) {
	s := &Server{app: cfg.App, store: cfg.Store, lib: cfg.Library, ai: cfg.AI, secure: cfg.SecureCookies, proxied: cfg.BehindProxy,
		pages: map[string]*template.Template{}, limiter: newLimiter(10, 15*time.Minute),
		speaking: newSpeechSlots(speechAtOnce, speechPerBusiness), voiceRate: newLimiter(speechPerMinute, time.Minute),
		audioUsed: newAudioUse(), voiceKey: seal.NewKey(),
		askRate: newLimiter(questionsPerMinute, time.Minute), limit: cmp.Or(cfg.RequestLimit, RequestLimit),
		pushKey: cfg.PushKey, turnOnRate: newLimiter(turnOnsPerHour, time.Hour), helloRate: newLimiter(hellosPerMinute, time.Minute)}
	if s.ai == nil {
		s.ai = &ai.Setup{USDtoINR: 88}
	}
	funcs := template.FuncMap{
		"initials": initials,
		"icon":     icon,
		// replaceSort returns a list query ("?f=…") with its order set to s, without the "?".
		"replaceSort": func(query, s string) string {
			vals, _ := url.ParseQuery(strings.TrimPrefix(query, "?"))
			vals.Del("s")
			if s != "recent" {
				vals.Set("s", s)
			}
			return vals.Encode()
		},
		// dict builds a map from key, value pairs, so one template block can be reused with
		// different data: {{template "x" (dict "Title" "…" "Lines" .Lines)}}.
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict needs key, value pairs")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict keys must be text")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
	}
	for _, name := range []string{"home", "login", "app", "offline", "settings", "password", "me", "welcome", "report"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(web.Files, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}
	static, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, err
	}
	s.static = http.StripPrefix("/static/", http.FileServerFS(static))
	if s.asset, err = fingerprint(static); err != nil {
		return nil, err
	}
	sw, err := fs.ReadFile(web.Files, "templates/sw.js")
	if err != nil {
		return nil, err
	}
	s.worker = bytes.ReplaceAll(sw, []byte("__VERSION__"), []byte(s.asset))
	return s, nil
}

// fingerprint is a short hash of every static file. Pages link to "/static/app.css?v=<fingerprint>",
// so when a file changes its address changes too, and browsers fetch the new one instead of an
// old copy they kept.
func fingerprint(files fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12], err
}

// Handler returns all routes, wrapped in the security layers: browsers may not send a form or
// request to Seyal from another site (cross-origin protection), and every response carries
// security headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", cacheFor(time.Hour, s.static))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("POST /{$}", s.login)
	mux.HandleFunc("POST /logout", s.logoutHere)
	mux.HandleFunc("GET /sw.js", s.serviceWorker)
	mux.HandleFunc("GET /offline", s.offline)
	mux.HandleFunc("GET /t/{slug}/manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /t/{slug}/login", s.loginPage)
	mux.HandleFunc("POST /t/{slug}/login", s.login)
	mux.HandleFunc("GET /t/{slug}/welcome/{code}", s.welcomePage)
	mux.HandleFunc("POST /t/{slug}/welcome/{code}", s.welcome)
	mux.HandleFunc("POST /t/{slug}/logout", s.authed(s.logout))
	mux.HandleFunc("POST /t/{slug}/logout-everywhere", s.authed(s.logoutEverywhere))
	mux.HandleFunc("GET /t/{slug}/me", s.authed(s.mePage))
	mux.HandleFunc("POST /t/{slug}/push/devices", s.authed(s.turnOnDevice))
	mux.HandleFunc("POST /t/{slug}/push/devices/{id}/off", s.authed(s.turnOffDevice))
	mux.HandleFunc("POST /t/{slug}/push/hello", s.authed(s.helloDevice))
	mux.HandleFunc("GET /t/{slug}/password", s.authed(s.passwordPage))
	mux.HandleFunc("POST /t/{slug}/password", s.authed(s.changePassword))
	mux.HandleFunc("GET /t/{slug}/{$}", s.authed(s.tellPage))
	mux.HandleFunc("POST /t/{slug}/tell", s.authed(s.tell))
	mux.HandleFunc("POST /t/{slug}/voice", s.authed(s.voice))
	mux.HandleFunc("POST /t/{slug}/move", s.authed(s.move))
	mux.HandleFunc("GET /t/{slug}/items/{id}", s.authed(s.itemPage))
	mux.HandleFunc("POST /t/{slug}/items/{id}/say", s.authed(s.say))
	mux.HandleFunc("GET /t/{slug}/items/{id}/report", s.authed(s.report))
	mux.HandleFunc("GET /t/{slug}/export/{process}", s.admin(s.exportItems))
	mux.HandleFunc("POST /t/{slug}/items/{id}/act", s.authed(s.act))
	mux.HandleFunc("POST /t/{slug}/items/{id}/field", s.authed(s.setField))
	mux.HandleFunc("POST /t/{slug}/items/{id}/decide", s.authed(s.decide))
	mux.HandleFunc("POST /t/{slug}/items/{id}/pass", s.authed(s.pass))
	mux.HandleFunc("POST /t/{slug}/items/{id}/take", s.authed(s.take))
	mux.HandleFunc("GET /t/{slug}/media/{id}", s.authed(s.media))

	// Settings: admins only.
	mux.HandleFunc("GET /t/{slug}/settings/{$}", s.admin(s.settingsHome))
	mux.HandleFunc("GET /t/{slug}/settings/people", s.admin(s.peoplePage))
	mux.HandleFunc("POST /t/{slug}/settings/people", s.admin(s.addPerson))
	mux.HandleFunc("POST /t/{slug}/settings/people/import", s.admin(s.importPeople))
	mux.HandleFunc("GET /t/{slug}/settings/people/export", s.admin(s.exportPeople))
	mux.HandleFunc("POST /t/{slug}/settings/master/{kind}/import", s.admin(s.importEntries))
	mux.HandleFunc("GET /t/{slug}/settings/master/{kind}/export", s.admin(s.exportEntries))
	mux.HandleFunc("POST /t/{slug}/settings/people/{id}", s.admin(s.updatePerson))
	mux.HandleFunc("POST /t/{slug}/settings/people/{id}/link", s.admin(s.newLink))
	mux.HandleFunc("GET /t/{slug}/settings/roles", s.admin(s.rolesPage))
	mux.HandleFunc("POST /t/{slug}/settings/roles", s.admin(s.addRole))
	mux.HandleFunc("GET /t/{slug}/settings/processes", s.admin(s.processesPage))
	mux.HandleFunc("POST /t/{slug}/settings/processes", s.admin(s.addProcess))
	mux.HandleFunc("GET /t/{slug}/settings/processes/{id}", s.admin(s.processPage))
	mux.HandleFunc("POST /t/{slug}/settings/processes/{id}", s.admin(s.changeProcess))
	mux.HandleFunc("GET /t/{slug}/settings/lists", s.admin(s.listsPage))
	mux.HandleFunc("POST /t/{slug}/settings/lists", s.admin(s.addList))
	mux.HandleFunc("GET /t/{slug}/settings/lists/{kind}", s.admin(s.listPage))
	mux.HandleFunc("POST /t/{slug}/settings/lists/{kind}", s.admin(s.changeList))
	mux.HandleFunc("GET /t/{slug}/settings/ai", s.admin(s.aiPage))
	mux.HandleFunc("GET /t/{slug}/settings/master/{kind}", s.admin(s.masterPage))
	mux.HandleFunc("POST /t/{slug}/settings/master/{kind}", s.admin(s.saveMaster))
	return securityHeaders(s.secure, http.NewCrossOriginProtection().Handler(mux))
}

// page is what every template gets.
type page struct {
	Asset          string // static files fingerprint, filled in by render
	Slug, Business string
	Session        *auth.Session
	Admin          bool   // may open Settings
	Notice         string // a one-time message, e.g. a new password
	Error          string
	Speech         bool // voice notes are turned into words in the composer (a speech service is set)
	Data           any
}

// adminRole is the role that may open Settings (people, roles, master data).
const adminRole = "admin"

// call is one request by a logged-in person.
type call struct {
	tenant  tenant
	session auth.Session
	token   string
}

func (c *call) page(data any) page {
	return page{Slug: c.tenant.Slug, Business: c.tenant.Name, Session: &c.session, Admin: c.admin(), Data: data}
}

func (c *call) admin() bool { return slices.Contains(c.session.Roles, adminRole) }

type tenant struct {
	ID, Slug, Name, Currency string
	Zone                     *time.Location
	Calendar                 chase.Calendar // its working days, for the days on Seyal's card hints (D77)
}

var errNoTenant = errors.New("no such business")

func (s *Server) tenant(r *http.Request) (tenant, error) {
	t := tenant{Slug: r.PathValue("slug")}
	var zone string
	var daysOff []int16
	err := s.app.QueryRow(r.Context(), `
		SELECT id::text, name, currency, timezone, days_off, ARRAY(SELECT to_char(d, 'YYYY-MM-DD') FROM unnest(holidays) d ORDER BY d)
		FROM tenants WHERE slug = $1`, t.Slug).
		Scan(&t.ID, &t.Name, &t.Currency, &zone, &daysOff, &t.Calendar.Holidays)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, errNoTenant
	}
	if err != nil {
		return t, err
	}
	if t.Zone, err = time.LoadLocation(zone); err != nil {
		t.Zone = time.UTC
	}
	t.Calendar.Zone = t.Zone
	for _, d := range daysOff {
		t.Calendar.DaysOff = append(t.Calendar.DaysOff, time.Weekday(d))
	}
	return t, nil
}

// authed lets a request through only with a login that reaches an active person in this business.
// Without one (or for an address that doesn't exist, so addresses can't be guessed) the person is
// sent to the login page; a login that isn't in this business goes on from there to "/". htmx
// requests get an HX-Redirect so the whole page changes.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		toLogin := func() {
			login := "/t/" + r.PathValue("slug") + "/login"
			// A page opened without a login (e.g. from a phone note) comes back after it.
			if r.Method == http.MethodGet && r.Header.Get("HX-Request") != "true" && safeNext(r.PathValue("slug"), r.URL.Path) {
				login += "?next=" + url.QueryEscape(r.URL.Path)
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", login)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, login, http.StatusSeeOther)
		}
		t, err := s.tenant(r)
		if errors.Is(err, errNoTenant) {
			toLogin()
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		c := &call{tenant: t, token: cookieValue(r, accountCookie)}
		var upgraded time.Time // when the new login session ends, if an old cookie was swapped for one
		err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
			// FOR ONE RELEASE (D64): an old per-business cookie becomes a login session.
			if old := cookieValue(r, legacyCookie); c.token == "" && old != "" {
				token, expires, err := auth.Upgrade(r.Context(), tx, old, r.UserAgent())
				switch {
				case err == nil:
					c.token, upgraded = token, expires
				case !errors.Is(err, auth.ErrNoSession):
					return err
				}
			}
			var err error
			c.session, err = auth.Check(r.Context(), tx, c.token, r.UserAgent())
			return err
		})
		if errors.Is(err, auth.ErrNoSession) {
			toLogin()
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !upgraded.IsZero() {
			s.setSession(w, c.token, upgraded)
			s.clearLegacy(w, t.Slug)
		}
		if cookieValue(r, lastCookie) != t.Slug {
			s.setLast(w, t.Slug)
		}
		// With a temporary password, the person chooses their own before anything else, and then goes
		// on to the page they opened.
		if pw := "/t/" + t.Slug + "/password"; c.session.MustChange && r.URL.Path != pw && !strings.HasSuffix(r.URL.Path, "/logout") {
			if r.Method == http.MethodGet && r.Header.Get("HX-Request") != "true" && safeNext(t.Slug, r.URL.Path) {
				pw += "?next=" + url.QueryEscape(r.URL.Path)
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", pw)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Redirect(w, r, pw, http.StatusSeeOther)
			return
		}
		h(w, r, c)
	}
}

// cookieValue is the value of the request's cookie called name, or "".
func cookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// homeView is "/" for someone logged in who isn't sent straight into a business.
type homeView struct {
	Email      string
	Businesses []auth.Business // active memberships only; none means "not in any business yet"
}

// home is the server's front door. Logged out, it is the login form (the same one as on every
// business address, D29). Logged in, it opens a business: the one last used on this device if the
// login is still an active member there, else the only one, else a plain list to choose from. Like
// the login page this is plumbing, not a process screen. It never sends anyone into a business they
// were disabled in, so there is no loop.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	var hv homeView
	var mine []auth.Business
	err := db.WithServer(r.Context(), s.app, func(tx pgx.Tx) error {
		a, err := auth.CheckAccount(r.Context(), tx, cookieValue(r, accountCookie))
		if err != nil {
			return err
		}
		hv.Email = a.Email
		mine, err = auth.Businesses(r.Context(), tx, a.ID)
		return err
	})
	if errors.Is(err, auth.ErrNoSession) {
		s.render(w, r, http.StatusOK, "login", page{Data: loginView{}})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, b := range mine {
		if b.Active {
			hv.Businesses = append(hv.Businesses, b)
		}
	}
	last := cookieValue(r, lastCookie)
	for _, b := range hv.Businesses {
		if len(hv.Businesses) == 1 || b.Slug == last {
			http.Redirect(w, r, "/t/"+b.Slug+"/", http.StatusSeeOther)
			return
		}
	}
	s.render(w, r, http.StatusOK, "home", page{Data: hv})
}

// serviceWorker is served from the root so it can look after every page. It is never cached
// long, so a new version reaches browsers on their next visit.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(s.worker)
}

func (s *Server) offline(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "offline", page{})
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// manifest describes the installable app for one business address. Like the login page, it is
// the same for every address and never names the business (D29). Browsers fetch it without the
// login cookie, so it can't require a login.
func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !slugPattern.MatchString(slug) {
		http.NotFound(w, r)
		return
	}
	base := "/t/" + slug + "/"
	m := map[string]any{
		"name": "Seyal", "short_name": "Seyal", "description": "No forms. Just tell Seyal.",
		"id": base, "start_url": base, "scope": base, "display": "standalone",
		"background_color": "#f7f7f8", "theme_color": "#f7f7f8",
		"icons": []map[string]string{
			{"src": "/static/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/static/icon-512.png", "sizes": "512x512", "type": "image/png"},
			{"src": "/static/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	json.NewEncoder(w).Encode(m)
}

// loginPage looks the same for every address, real or not, and never shows the business's name.
// Someone already logged in goes on into the business if their login is in it, and to "/" if not
// (where they choose), whether the address is real or not.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	next := r.URL.Query().Get("next")
	if !safeNext(slug, next) {
		next = ""
	}
	if token := cookieValue(r, accountCookie); token != "" {
		var member bool
		err := db.WithServer(r.Context(), s.app, func(tx pgx.Tx) error {
			a, err := auth.CheckAccount(r.Context(), tx, token)
			if err != nil {
				return err
			}
			member, err = activeIn(r, tx, a.ID, slug)
			return err
		})
		switch {
		case err == nil && member:
			http.Redirect(w, r, cmp.Or(next, "/t/"+slug+"/"), http.StatusSeeOther)
			return
		case err == nil:
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		case !errors.Is(err, auth.ErrNoSession):
			s.fail(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "login", page{Slug: slug, Data: loginView{Next: next}})
}

// loginView is the login form: the email typed (shown again after a wrong password), and the safe
// "go back to" path (safeNext), if any.
type loginView struct {
	Email, Next string
}

// activeIn reports whether the login reaches an active person in the business at slug.
func activeIn(r *http.Request, tx pgx.Tx, accountID, slug string) (bool, error) {
	bs, err := auth.Businesses(r.Context(), tx, accountID)
	return slices.ContainsFunc(bs, func(b auth.Business) bool { return b.Slug == slug && b.Active }), err
}

// login answers the one login form, on "/" and on every business address. The address is never
// looked up before the password is checked, so a made-up address, a real one the login isn't in
// and an unknown email all answer exactly as a wrong password does, in the same time (D29). After a
// good password the login goes into the business at this address if it is in it, else to "/".
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug") // "" on "/"
	email, password := strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("password")
	next := r.PostFormValue("next")
	if !safeNext(slug, next) {
		next = "" // an outside address, or another business's, is never followed
	}
	p := page{Slug: slug, Data: loginView{Email: email, Next: next}}
	// Failed logins are counted per visitor's address and per email, over every page with the form:
	// guessing at one person's login from many addresses, or on many business addresses, is slowed
	// down too.
	key, who := "ip|"+s.clientIP(r), "email|"+strings.ToLower(email)
	if !s.limiter.take(key, who) {
		p.Error = "Too many failed attempts. Please wait 15 minutes and try again."
		s.render(w, r, http.StatusTooManyRequests, "login", p)
		return
	}
	var token string
	var a auth.Account
	var member bool
	err := db.WithServer(r.Context(), s.app, func(tx pgx.Tx) error {
		var err error
		if token, a, err = auth.LogIn(r.Context(), tx, email, password, r.UserAgent()); err != nil {
			return err
		}
		member, err = activeIn(r, tx, a.ID, slug)
		return err
	})
	if errors.Is(err, auth.ErrBadLogin) { // the attempt stays counted
		p.Error = "Wrong email or password."
		s.render(w, r, http.StatusUnauthorized, "login", p)
		return
	}
	s.limiter.refund(key, who) // the password was right (or an error came before it was checked)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSession(w, token, a.ExpiresAt)
	if member {
		http.Redirect(w, r, cmp.Or(next, "/t/"+slug+"/"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logout ends the login session, in every business, and goes back to this address's login page
// (inside the installed app).
func (s *Server) logout(w http.ResponseWriter, r *http.Request, c *call) {
	if err := s.endLogin(r, c.token); err != nil {
		s.fail(w, r, err)
		return
	}
	s.clearSession(w, c.tenant.Slug)
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/login", http.StatusSeeOther)
}

// logoutHere is logging out on "/", e.g. with a login that isn't in any business yet.
func (s *Server) logoutHere(w http.ResponseWriter, r *http.Request) {
	if err := s.endLogin(r, cookieValue(r, accountCookie)); err != nil {
		s.fail(w, r, err)
		return
	}
	s.clearSession(w, "")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// endLogin ends a login session, if there is one.
func (s *Server) endLogin(r *http.Request, token string) error {
	if token == "" {
		return nil
	}
	return db.WithServer(r.Context(), s.app, func(tx pgx.Tx) error {
		return auth.LogOut(r.Context(), tx, token)
	})
}

// setSession puts the login cookie, for every address on the server.
func (s *Server) setSession(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: accountCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// clearSession removes the login cookie, and also the old cookie of the business address at slug
// if one is given.
func (s *Server) clearSession(w http.ResponseWriter, slug string) {
	http.SetCookie(w, &http.Cookie{
		Name: accountCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	if slug != "" {
		s.clearLegacy(w, slug)
	}
}

// clearLegacy removes a business address's old login cookie (FOR ONE RELEASE, D64).
func (s *Server) clearLegacy(w http.ResponseWriter, slug string) {
	http.SetCookie(w, &http.Cookie{
		Name: legacyCookie, Value: "", Path: "/t/" + slug + "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// setLast remembers on this device the business just used, where "/" goes next time. It holds only
// the address, which whoever uses the device already knows.
func (s *Server) setLast(w http.ResponseWriter, slug string) {
	http.SetCookie(w, &http.Cookie{
		Name: lastCookie, Value: slug, Path: "/", MaxAge: 365 * 24 * 60 * 60,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// render runs a page template into a buffer first, so a template error never sends half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	p.Asset = s.asset
	_, p.Speech = s.ai.Speech()
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// fail logs the real error and shows the person a plain message, never the details.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
}

func securityHeaders(https bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if https { // browsers then never try plain HTTP for this site again
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
			"style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=(self)")
		next.ServeHTTP(w, r)
	})
}

func cacheFor(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(d.Seconds())))
		next.ServeHTTP(w, r)
	})
}

// clientIP is the address the request came from. Behind our own reverse proxy that is the last address
// the proxy added to X-Forwarded-For (anything before it was sent by the visitor's browser and can't be trusted).
func (s *Server) clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); s.proxied && len(xff) > 0 {
		parts := strings.Split(xff[len(xff)-1], ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter counts password attempts per key and blocks a key after max failed ones within the window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, seen: map[string][]time.Time{}}
}

// take reports whether every key may try a password now, and if so counts the attempt for each key
// at once, before the password is checked, in one locked step. So guesses sent at the same moment
// can't all get past the check before any of them is counted. A good password gives its attempt
// back (refund); a wrong one stays counted.
func (l *limiter) take(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		if len(l.recent(key)) >= l.max {
			return false
		}
	}
	now := time.Now()
	for _, key := range keys {
		l.seen[key] = append(l.recent(key), now)
	}
	return true
}

// refund gives back one attempt per key: the password was right, or wasn't checked after all.
func (l *limiter) refund(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		if kept := l.recent(key); len(kept) > 1 {
			l.seen[key] = kept[:len(kept)-1]
		} else {
			delete(l.seen, key)
		}
	}
}

// recent drops failures older than the window. The caller holds the lock.
func (l *limiter) recent(key string) []time.Time {
	cutoff := time.Now().Add(-l.window)
	kept := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.seen, key)
		return nil
	}
	l.seen[key] = kept
	return kept
}
