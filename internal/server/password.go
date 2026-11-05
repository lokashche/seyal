// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// Changing your own password: from your name in the left column, or straight after logging in with a
// temporary password (one an admin made, before set-your-password links). It is the login's password,
// so it changes in every business the login belongs to. Like the login page, this is account
// plumbing, not a process screen. A forgotten password: an admin makes a new set-your-password link
// (welcome.go).

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.render(w, r, http.StatusOK, "password", c.page(c.session.MustChange))
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, c *call) {
	current, next := r.PostFormValue("current"), r.PostFormValue("next")
	problem := func(msg string) {
		p := c.page(c.session.MustChange)
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
	switch {
	case errors.Is(err, auth.ErrWrongPassword), errors.Is(err, auth.ErrSamePassword):
		problem(capital(err.Error()) + ".")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.setSession(w, token, a.ExpiresAt)
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/", http.StatusSeeOther)
}
