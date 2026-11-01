// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server is Seyal's web app: one working screen with three panes (My Work, the thread,
// fields & approvals), following docs/seyal-concept.html, plus login. Each business has its own
// address, /t/<slug>/.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
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
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/web"
)

const cookieName = "seyal_session"

// Server serves the web app.
type Server struct {
	app     *pgxpool.Pool
	store   media.Store
	lib     *playbook.Library // master-kind definitions, for the Settings forms
	ai      *ai.Setup
	secure  bool // send cookies only over HTTPS
	pages   map[string]*template.Template
	static  http.Handler
	asset   string // fingerprint of the static files, added to their addresses
	worker  []byte // the service worker script, with the fingerprint filled in
	limiter *limiter
}

// Config is what the web app needs.
type Config struct {
	App           *pgxpool.Pool     // the app role's connections
	Store         media.Store       // voice notes and files
	Library       *playbook.Library // templates and master-kind definitions (Settings forms)
	AI            *ai.Setup         // models per purpose; nil means AI is off
	SecureCookies bool              // true when the app is reached over HTTPS
}

// New prepares the web app.
func New(cfg Config) (*Server, error) {
	s := &Server{app: cfg.App, store: cfg.Store, lib: cfg.Library, ai: cfg.AI, secure: cfg.SecureCookies,
		pages: map[string]*template.Template{}, limiter: newLimiter(10, 15*time.Minute)}
	if s.ai == nil {
		s.ai = &ai.Setup{USDtoINR: 88}
	}
	funcs := template.FuncMap{
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
	for _, name := range []string{"home", "login", "app", "offline", "settings"} {
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
	mux.HandleFunc("GET /sw.js", s.serviceWorker)
	mux.HandleFunc("GET /offline", s.offline)
	mux.HandleFunc("GET /t/{slug}/manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /t/{slug}/login", s.loginPage)
	mux.HandleFunc("POST /t/{slug}/login", s.login)
	mux.HandleFunc("POST /t/{slug}/logout", s.authed(s.logout))
	mux.HandleFunc("GET /t/{slug}/{$}", s.authed(s.tellPage))
	mux.HandleFunc("POST /t/{slug}/tell", s.authed(s.tell))
	mux.HandleFunc("GET /t/{slug}/items/{id}", s.authed(s.itemPage))
	mux.HandleFunc("POST /t/{slug}/items/{id}/say", s.authed(s.say))
	mux.HandleFunc("POST /t/{slug}/items/{id}/act", s.authed(s.act))
	mux.HandleFunc("POST /t/{slug}/items/{id}/decide", s.authed(s.decide))
	mux.HandleFunc("GET /t/{slug}/media/{id}", s.authed(s.media))

	// Settings: admins only.
	mux.HandleFunc("GET /t/{slug}/settings/{$}", s.admin(s.settingsHome))
	mux.HandleFunc("GET /t/{slug}/settings/people", s.admin(s.peoplePage))
	mux.HandleFunc("POST /t/{slug}/settings/people", s.admin(s.addPerson))
	mux.HandleFunc("POST /t/{slug}/settings/people/{id}", s.admin(s.updatePerson))
	mux.HandleFunc("POST /t/{slug}/settings/people/{id}/password", s.admin(s.resetPassword))
	mux.HandleFunc("GET /t/{slug}/settings/roles", s.admin(s.rolesPage))
	mux.HandleFunc("POST /t/{slug}/settings/roles", s.admin(s.addRole))
	mux.HandleFunc("GET /t/{slug}/settings/processes", s.admin(s.processesPage))
	mux.HandleFunc("POST /t/{slug}/settings/processes", s.admin(s.addProcess))
	mux.HandleFunc("GET /t/{slug}/settings/processes/{id}", s.admin(s.processPage))
	mux.HandleFunc("POST /t/{slug}/settings/processes/{id}", s.admin(s.changeProcess))
	mux.HandleFunc("GET /t/{slug}/settings/ai", s.admin(s.aiPage))
	mux.HandleFunc("GET /t/{slug}/settings/master/{kind}", s.admin(s.masterPage))
	mux.HandleFunc("POST /t/{slug}/settings/master/{kind}", s.admin(s.saveMaster))
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

// page is what every template gets.
type page struct {
	Asset          string // static files fingerprint, filled in by render
	Slug, Business string
	Session        *auth.Session
	Admin          bool   // may open Settings
	Notice         string // a one-time message, e.g. a new password
	Error          string
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
}

var errNoTenant = errors.New("no such business")

func (s *Server) tenant(r *http.Request) (tenant, error) {
	t := tenant{Slug: r.PathValue("slug")}
	var zone string
	err := s.app.QueryRow(r.Context(), "SELECT id::text, name, currency, timezone FROM tenants WHERE slug = $1", t.Slug).
		Scan(&t.ID, &t.Name, &t.Currency, &zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, errNoTenant
	}
	if err != nil {
		return t, err
	}
	if t.Zone, err = time.LoadLocation(zone); err != nil {
		t.Zone = time.UTC
	}
	return t, nil
}

// authed lets a request through only with a valid session for this business. Without one (or for
// an address that doesn't exist, so addresses can't be guessed) the person is sent to the login
// page. htmx requests get an HX-Redirect so the whole page changes.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		toLogin := func() {
			login := "/t/" + r.PathValue("slug") + "/login"
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
		c := &call{tenant: t}
		if cookie, err := r.Cookie(cookieName); err == nil {
			c.token = cookie.Value
		}
		err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
			var err error
			c.session, err = auth.Check(r.Context(), tx, c.token)
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
		h(w, r, c)
	}
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "home", page{})
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
		"background_color": "#f4f5f1", "theme_color": "#c9861a",
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
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "login", page{Slug: r.PathValue("slug"), Data: ""})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	email, password := strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("password")
	p := page{Slug: slug, Data: email}
	key := slug + "|" + clientIP(r)
	if !s.limiter.allowed(key) {
		p.Error = "Too many failed attempts. Please wait 15 minutes and try again."
		s.render(w, r, http.StatusTooManyRequests, "login", p)
		return
	}
	wrong := func() {
		s.limiter.failed(key)
		p.Error = "Wrong email or password."
		s.render(w, r, http.StatusUnauthorized, "login", p)
	}

	t, err := s.tenant(r)
	if errors.Is(err, errNoTenant) {
		auth.Pretend(password) // same time and answer as a real address
		wrong()
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var token string
	var sess auth.Session
	err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
		var err error
		token, sess, err = auth.Login(r.Context(), tx, email, password, r.UserAgent())
		return err
	})
	if errors.Is(err, auth.ErrBadLogin) {
		wrong()
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/t/" + t.Slug + "/", Expires: sess.ExpiresAt,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/t/"+t.Slug+"/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, c *call) {
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		return auth.Logout(r.Context(), tx, c.token)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/t/" + c.tenant.Slug + "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/login", http.StatusSeeOther)
}

// render runs a page template into a buffer first, so a template error never sends half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	p.Asset = s.asset
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
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

// clientIP is the address the request came from. (Behind a reverse proxy this will need the
// proxy's forwarded address instead; see the deploy step.)
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter counts failed logins per key and blocks a key after max failures within the window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, seen: map[string][]time.Time{}}
}

func (l *limiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key)) < l.max
}

func (l *limiter) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[key] = append(l.recent(key), time.Now())
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
