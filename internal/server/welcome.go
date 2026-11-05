// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// Set-your-password links: an admin adds a person (or makes a new link for one who forgot their
// password) and sends the link through any chat app. The link opens the welcome page, where the person
// chooses a password and is logged in. Account plumbing like the login page, not a process screen.

// invite is a link just made, shown once to the admin to send on.
type invite struct {
	Name, URL, Message string
	code               string
}

// invites fills in the full addresses and the message to send, and shows them on the People page.
func (s *Server) invites(r *http.Request, c *call, list ...invite) func(sv *settingsView) {
	scheme := "http"
	if s.secure || r.TLS != nil {
		scheme = "https"
	}
	for i, in := range list {
		list[i].URL = scheme + "://" + r.Host + "/t/" + c.tenant.Slug + "/welcome/" + in.code
		list[i].Message = fmt.Sprintf("Hi %s, here is your link to set up Seyal for %s. It works once, for %d hours: %s",
			in.Name, c.tenant.Name, int(auth.LinkLength.Hours()), list[i].URL)
	}
	return func(sv *settingsView) {
		if sv.People != nil {
			sv.People.Invites = list
		}
	}
}

// welcomeView is a link's page. When Elsewhere, the email's login is also used in another business,
// so this business's link can't set its password (D64); the page says who can help instead.
type welcomeView struct {
	auth.Holder
	Bad bool // unknown, used, replaced or expired: the page says to ask for a new link
}

func (s *Server) welcomePage(w http.ResponseWriter, r *http.Request) {
	s.showWelcome(w, r, http.StatusOK, "")
}

// welcome sets the login's password from a link and logs the person in.
func (s *Server) welcome(w http.ResponseWriter, r *http.Request) {
	next := r.PostFormValue("next")
	if next != r.PostFormValue("again") {
		s.showWelcome(w, r, http.StatusUnprocessableEntity, "The two passwords are different.")
		return
	}
	if err := auth.CheckNew(next); err != nil {
		s.showWelcome(w, r, http.StatusUnprocessableEntity, capital(err.Error())+".")
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
	var token string
	var expires time.Time
	err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
		var err error
		token, expires, err = auth.UseLink(r.Context(), tx, r.PathValue("code"), next, r.UserAgent())
		return err
	})
	switch {
	case errors.Is(err, auth.ErrBadLink):
		s.showWelcome(w, r, http.StatusNotFound, "")
		return
	case errors.Is(err, auth.ErrLinkedElsewhere): // it became so after the page was shown
		s.showWelcome(w, r, http.StatusConflict, "")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.setSession(w, token, expires)
	http.Redirect(w, r, "/t/"+t.Slug+"/", http.StatusSeeOther)
}

// showWelcome shows the form for a link that works, or "ask for a new link". A link that doesn't
// work, and an address that doesn't exist, look the same (like the login page, D29).
func (s *Server) showWelcome(w http.ResponseWriter, r *http.Request, status int, problem string) {
	w.Header().Set("Cache-Control", "no-store") // the address is a key; keep the page out of caches
	p := page{Slug: r.PathValue("slug"), Error: problem}
	var wv welcomeView
	t, err := s.tenant(r)
	if err == nil {
		err = db.WithTenant(r.Context(), s.app, t.ID, func(tx pgx.Tx) error {
			var err error
			wv.Holder, err = auth.LinkHolder(r.Context(), tx, r.PathValue("code"))
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
