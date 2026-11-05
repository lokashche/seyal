// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// Set-your-password links: an admin adds a person (or makes a new link for one who forgot their
// password) and sends the link through any chat app. The link opens the welcome page, where the person
// chooses a password, or, if their email already has a login that isn't theirs here (e.g. one used in
// another business), logs in with it to add this business (D64). Account plumbing like the login
// page, not a process screen.

// invite is a link just made, shown once to the admin to send on.
type invite struct {
	Name, URL, Message string
	code               string
}

// invites fills in the full addresses and the message to send, and shows them on the People page. The
// message is the same whatever the link will ask (choose a first password, a new one, or join), so
// this business isn't told who uses Seyal elsewhere.
func (s *Server) invites(r *http.Request, c *call, list ...invite) func(sv *settingsView) {
	scheme := "http"
	if s.secure || r.TLS != nil {
		scheme = "https"
	}
	for i, in := range list {
		list[i].URL = scheme + "://" + r.Host + "/t/" + c.tenant.Slug + "/welcome/" + in.code
		list[i].Message = fmt.Sprintf("Hi %s, here is your Seyal link for %s. It works once, for %d hours: %s",
			in.Name, c.tenant.Name, int(auth.LinkLength.Hours()), list[i].URL)
	}
	return func(sv *settingsView) {
		if sv.People != nil {
			sv.People.Invites = list
		}
	}
}

// welcomeView is a link's page: its person, and what the link asks of them (Mode).
type welcomeView struct {
	auth.Holder
	Bad bool // unknown, used, replaced or expired: the page says to ask for a new link
}

func (s *Server) welcomePage(w http.ResponseWriter, r *http.Request) {
	s.showWelcome(w, r, http.StatusOK, "")
}

// welcome uses a link: it sets the login's password (ModeChoose) or adds this business to the login
// (ModeJoin), and the person is logged in on this device. The form says which mode its page was
// shown with; if that changed meanwhile, nothing happens and the page asks to open the link again.
func (s *Server) welcome(w http.ResponseWriter, r *http.Request) {
	u := auth.LinkUse{Mode: auth.Mode(r.PostFormValue("mode")), Token: cookieValue(r, accountCookie), UserAgent: r.UserAgent()}
	switch u.Mode {
	case auth.ModeChoose:
		u.Password = r.PostFormValue("next")
		if u.Password != r.PostFormValue("again") {
			s.showWelcome(w, r, http.StatusUnprocessableEntity, "The two passwords are different.")
			return
		}
		if err := auth.CheckNew(u.Password); err != nil {
			s.showWelcome(w, r, http.StatusUnprocessableEntity, capital(err.Error())+".")
			return
		}
	case auth.ModeJoin:
		u.Password = r.PostFormValue("password")
		// Logged out, only the password shows the login is theirs. An empty one is refused here,
		// before the database is asked, and isn't counted by the limiter: it guesses nothing.
		// (With a cookie, one tap needs no password; UseLink decides.)
		if u.Password == "" && u.Token == "" {
			s.showWelcome(w, r, http.StatusUnprocessableEntity, "Please enter your password.")
			return
		}
	default: // an old page, or a made-up form
		s.showWelcome(w, r, http.StatusConflict, "Please open the link again.")
		return
	}
	t, err := s.tenant(r)
	if errors.Is(err, errNoTenant) {
		s.showWelcome(w, r, http.StatusNotFound, "")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A join with a password is a login: its attempts are counted with the login form's, per
	// visitor's address and per email (the link's, so typing another one changes nothing).
	ip, who := "ip|"+s.clientIP(r), ""
	limited := false
	var token string
	var expires time.Time
	err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
		if u.Mode == auth.ModeJoin && u.Password != "" {
			h, err := auth.LinkHolder(r.Context(), tx, r.PathValue("code"), "")
			if err != nil {
				return err
			}
			who = "email|" + strings.ToLower(h.Email)
			if limited = !s.limiter.take(ip, who); limited {
				who = "" // nothing was counted
				return nil
			}
		}
		var err error
		token, expires, err = auth.UseLink(r.Context(), tx, r.PathValue("code"), u)
		return err
	})
	if who != "" && !errors.Is(err, auth.ErrWrongPassword) {
		s.limiter.refund(ip, who) // only a wrong password stays counted
	}
	switch {
	case limited:
		s.showWelcome(w, r, http.StatusTooManyRequests, "Too many failed attempts. Please wait 15 minutes and try again.")
		return
	case errors.Is(err, auth.ErrWrongPassword):
		s.showWelcome(w, r, http.StatusUnauthorized, "Wrong password.")
		return
	case errors.Is(err, auth.ErrBadLink):
		s.showWelcome(w, r, http.StatusNotFound, "")
		return
	case errors.Is(err, auth.ErrLinkedElsewhere): // it became so after the page was shown
		s.showWelcome(w, r, http.StatusConflict, "")
		return
	case errors.Is(err, auth.ErrLinkChanged):
		s.showWelcome(w, r, http.StatusConflict, "This link changed after you opened it, so nothing was saved. Please open the link again.")
		return
	case errors.Is(err, auth.ErrOtherLogin):
		s.showWelcome(w, r, http.StatusConflict, "")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.setSession(w, token, expires)
	http.Redirect(w, r, "/t/"+t.Slug+"/", http.StatusSeeOther)
}

// showWelcome shows what a link that works asks for, or "ask for a new link". A link that doesn't
// work, and an address that doesn't exist, look the same (like the login page, D29).
func (s *Server) showWelcome(w http.ResponseWriter, r *http.Request, status int, problem string) {
	w.Header().Set("Cache-Control", "no-store") // the address is a key; keep the page out of caches
	p := page{Slug: r.PathValue("slug"), Error: problem}
	var wv welcomeView
	t, err := s.tenant(r)
	if err == nil {
		err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
			var err error
			wv.Holder, err = auth.LinkHolder(r.Context(), tx, r.PathValue("code"), cookieValue(r, accountCookie))
			return err
		})
	}
	switch {
	case errors.Is(err, errNoTenant), errors.Is(err, auth.ErrBadLink):
		wv, p.Error = welcomeView{Bad: true}, ""
		if status == http.StatusOK {
			status = http.StatusNotFound
		}
	case err != nil:
		s.fail(w, r, err)
		return
	default:
		p.Business = t.Name
	}
	p.Data = wv
	s.render(w, r, status, "welcome", p)
}
