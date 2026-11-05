// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
)

// The Me page: who you are in this business (name, roles), your login (its email; change password,
// log out, log out on all devices) and, when the login belongs to several businesses, plain links to
// switch between them: the login cookie works in all of them, so switching needs no new login. Account
// plumbing like the password page, not a process screen. Your name and roles are changed by an admin
// of the business in Settings → People, as a ledger event.

type profileView struct {
	Roles      []string        // role names, e.g. "Sales"
	Businesses []auth.Business // the login's active businesses, shown only when there are two or more
}

func (s *Server) mePage(w http.ResponseWriter, r *http.Request, c *call) {
	var pv profileView
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		rows, _ := tx.Query(r.Context(), `
			SELECT r.name FROM person_roles pr
			JOIN roles r ON r.tenant_id = pr.tenant_id AND r.key = pr.role_key
			WHERE pr.tenant_id = current_tenant() AND pr.person_id::text = $1 AND pr.revoked_at IS NULL
			ORDER BY r.name`, c.session.PersonID)
		var err error
		if pv.Roles, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		mine, err := auth.Businesses(r.Context(), tx, c.session.AccountID)
		for _, b := range mine {
			if b.Active {
				pv.Businesses = append(pv.Businesses, b)
			}
		}
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(pv.Businesses) < 2 {
		pv.Businesses = nil
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
