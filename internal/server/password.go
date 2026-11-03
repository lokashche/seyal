// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// Changing your own password: from your name in the left column, or straight after logging in with a
// password an admin made (a temporary one). Like the login page, this is account plumbing, not a
// process screen. A forgotten password: an admin makes a new set-your-password link (welcome.go).

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
	var email, token string
	var sess auth.Session
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		if err := auth.ChangePassword(r.Context(), tx, c.session.PersonID, current, next); err != nil {
			return err
		}
		// Every session ended with the old password; this device carries on with a new one.
		if err := tx.QueryRow(r.Context(), `SELECT login_email FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			c.session.PersonID).Scan(&email); err != nil {
			return err
		}
		var err error
		token, sess, err = auth.Login(r.Context(), tx, email, next, r.UserAgent())
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
	s.setSession(w, c.tenant.Slug, token, sess.ExpiresAt)
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/", http.StatusSeeOther)
}

// setSession puts the login cookie for one business address.
func (s *Server) setSession(w http.ResponseWriter, slug, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/t/" + slug + "/", Expires: expires,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}
