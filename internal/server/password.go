// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"cmp"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// Changing your own password: from Me, or straight after logging in with a temporary password.
// Nothing makes temporary passwords any more; the only ones left were made by an admin before
// set-your-password links and carried over to logins by migration 00015. It is the login's password,
// so it changes in every business the login belongs to. Like the login page, this is account
// plumbing, not a process screen. A forgotten password: if the login is used in one business alone,
// an admin there makes a new set-your-password link (welcome.go); otherwise only the server's
// operator can reset it (`seyal set-password`, D64).

// passwordView is the password form: whether it is the first choice after a temporary password, and
// the safe "go back to" path (safeNext) to go on to afterwards, if any. Push says phone notes are
// switched on for this server: a new password stops them on every device, this one too, and the page
// says so.
type passwordView struct {
	First bool
	Next  string
	Push  bool
}

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.render(w, r, http.StatusOK, "password", c.page(s.passwordView(c, r.URL.Query().Get("next"))))
}

func (s *Server) passwordView(c *call, next string) passwordView {
	if !safeNext(c.tenant.Slug, next) {
		next = ""
	}
	return passwordView{First: c.session.MustChange, Next: next, Push: s.pushKey != ""}
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, c *call) {
	current, next := r.PostFormValue("current"), r.PostFormValue("next")
	view := s.passwordView(c, r.PostFormValue("then"))
	problem := func(msg string) {
		p := c.page(view)
		p.Error = msg
		s.render(w, r, http.StatusUnprocessableEntity, "password", p)
	}
	if next != r.PostFormValue("again") {
		problem("The two new passwords are different.")
		return
	}
	if err := auth.CheckNew(next); err != nil {
		problem(capital(err.Error()) + ".")
		return
	}
	// Checking the current password is a login: its wrong guesses count with the login form's, per
	// visitor's address and per the login's email, so a session left open on a shared computer
	// can't be used to guess the password faster than the login form allows.
	ip, who := "ip|"+s.clientIP(r), "email|"+strings.ToLower(c.session.Email)
	if !s.limiter.take(ip, who) {
		p := c.page(view)
		p.Error = "Too many failed attempts. Please wait 15 minutes and try again."
		s.render(w, r, http.StatusTooManyRequests, "password", p)
		return
	}
	var token string
	var a auth.Account
	err := db.WithServer(r.Context(), s.app, func(tx pgx.Tx) error {
		if err := auth.ChangePassword(r.Context(), tx, c.session.AccountID, current, next); err != nil {
			return err
		}
		// Every session of the login ended with the old password, in every business; this device
		// carries on with a new one.
		var err error
		token, a, err = auth.LogIn(r.Context(), tx, c.session.Email, next, r.UserAgent())
		return err
	})
	if !errors.Is(err, auth.ErrWrongPassword) {
		s.limiter.refund(ip, who) // only a wrong current password stays counted
	}
	switch {
	case errors.Is(err, auth.ErrWrongPassword), errors.Is(err, auth.ErrSamePassword):
		problem(capital(err.Error()) + ".")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.setSession(w, token, a.ExpiresAt)
	http.Redirect(w, r, cmp.Or(view.Next, "/t/"+c.tenant.Slug+"/"), http.StatusSeeOther)
}
