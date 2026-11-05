// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// Who may change an item (D55): whoever has it now (the person it is assigned to, or anyone in the
// team it is with), whoever started it, and admins. Everyone in the business may still read every
// item and write notes in its thread; approvals follow their own rules (the roles in the process).
// Seyal acting on its own (a rule, linked work reporting back) may always change an item; Seyal
// acting for a person may do what that person may.

// AdminRole is the role that may change any item (and open Settings).
const AdminRole = "admin"

// ErrNotYours means the person may read the item and write in it, but not change it.
var ErrNotYours = errors.New("only whoever has this item, whoever started it, or an admin can change it; you can still write a note")

// MayChange reports, as nil or ErrNotYours, whether the actor may change the item's details or
// take one of its actions. tx must come from db.WithTenant.
func MayChange(ctx context.Context, tx pgx.Tx, it Item, by ledger.Actor) error {
	person := by.PersonID
	if by.Kind == ledger.Agent {
		person = by.OnBehalfOf
	}
	if by.Kind == ledger.System || person == "" || person == it.AssigneeID || person == it.CreatedBy {
		return nil
	}
	rows, _ := tx.Query(ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL`, person)
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("workitem: roles: %w", err)
	}
	if slices.Contains(roles, AdminRole) || (it.Team != "" && slices.Contains(roles, it.Team)) {
		return nil
	}
	return ErrNotYours
}

// Visible keeps the items the person may see, for everything that shows or answers about many items
// at once, such as asking Seyal about the work (D67). Today that is every item of the business (D55):
// its work is open to everyone in it. A person who isn't active in this business sees nothing. A later
// rule (for example "only your team's work") belongs here, so every answer follows it at once.
// tx must come from db.WithTenant.
func Visible(ctx context.Context, tx pgx.Tx, personID string, items []Item) ([]Item, error) {
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM persons WHERE tenant_id = current_tenant() AND id::text = $1 AND status = 'active')`,
		personID).Scan(&active); err != nil {
		return nil, fmt.Errorf("workitem: visible: %w", err)
	}
	if !active {
		return nil, nil
	}
	return items, nil
}
