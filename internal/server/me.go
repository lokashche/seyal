// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// The Me page: who you are in this business (name, login email, roles), and your login (change
// password, log out, log out on all devices). Account plumbing like the password page, not a process
// screen. Your name and email are changed by an admin in Settings → People, as a ledger event.

type profileView struct {
	Email string
	Roles []string // role names, e.g. "Sales"
}

func (s *Server) mePage(w http.ResponseWriter, r *http.Request, c *call) {
	var pv profileView
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `
			SELECT coalesce(login_email, '') FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			c.session.PersonID).Scan(&pv.Email); err != nil {
			return err
		}
		rows, _ := tx.Query(r.Context(), `
			SELECT r.name FROM person_roles pr
			JOIN roles r ON r.tenant_id = pr.tenant_id AND r.key = pr.role_key
			WHERE pr.tenant_id = current_tenant() AND pr.person_id::text = $1 AND pr.revoked_at IS NULL
			ORDER BY r.name`, c.session.PersonID)
		var err error
		pv.Roles, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "me", c.page(pv))
}

// logoutEverywhere ends every session of the login, this one included, in every business.
func (s *Server) logoutEverywhere(w http.ResponseWriter, r *http.Request, c *call) {
	err := db.WithServer(r.Context(), s.app, func(tx pgx.Tx) error {
		return auth.LogOutEverywhere(r.Context(), tx, c.session.AccountID)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.clearSession(w, c.tenant.Slug)
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/login", http.StatusSeeOther)
}
