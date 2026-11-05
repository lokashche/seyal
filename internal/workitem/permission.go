// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
)

// Who may change an item (D55, narrowed by D68): whoever has it now (the person it is assigned to, or
// anyone in the team it is with), whoever started it, and admins. In a step owned by a role, a person
// who has the item but lost that role no longer counts as having it. Everyone in the business may still
// read every item and write notes in its thread; approvals follow their own rules (the roles in the
// process). A process may say per move who may take it ("who:", see engine.Allowed). Seyal acting on
// its own (a rule, linked work reporting back) may always change an item; Seyal acting for a person may
// do what that person may.

// AdminRole is the role that may change any item (and open Settings).
const AdminRole = "admin"

// ErrNotYours means the person may read the item and write in it, but not change it.
var ErrNotYours = errors.New("only whoever has this item, whoever started it, or an admin can change it; you can still write a note")

// NotAllowedError says who may take a move the person may not take. It counts as ErrNotYours.
type NotAllowedError struct {
	Action string
	Who    string // in plain words, e.g. "whoever has it"
}

func (e *NotAllowedError) Error() string {
	return fmt.Sprintf("only %s can do %q here; you can still write a note", e.Who, strings.ReplaceAll(e.Action, "_", " "))
}

func (e *NotAllowedError) Unwrap() error { return ErrNotYours }

// MayChange reports, as nil or ErrNotYours, whether the actor may change the item's details. Moves
// have their own who words, which Act checks. tx must come from db.WithTenant.
func MayChange(ctx context.Context, tx pgx.Tx, it Item, by ledger.Actor) error {
	if by.Kind == ledger.System {
		return nil
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return err
	}
	a, err := s.actor(by)
	if err != nil {
		return err
	}
	if ok, _ := engine.Allowed(s.p, s.engineItem(it), nil, a); !ok {
		return ErrNotYours
	}
	return nil
}

// Actor is the engine's view of a person acting in this business: their roles, and whether they are
// an admin. Screens use it to show only the moves the person may take.
func Actor(personID string, roles []string) engine.Actor {
	return engine.Actor{Person: engine.Person{ID: personID, Roles: roles}, Admin: slices.Contains(roles, AdminRole)}
}

// actor turns who is acting into the engine's view of them, with the person's current roles. An
// actor that isn't Seyal on its own and has no person gets no person, so every who word refuses it.
func (s session) actor(by ledger.Actor) (engine.Actor, error) {
	if by.Kind == ledger.System {
		return engine.Actor{System: true}, nil
	}
	person := by.PersonID
	if by.Kind == ledger.Agent {
		person = by.OnBehalfOf
	}
	if person == "" {
		return engine.Actor{}, nil
	}
	rows, _ := s.tx.Query(s.ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL`, person)
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return engine.Actor{}, fmt.Errorf("workitem: roles: %w", err)
	}
	return Actor(person, roles), nil
}

// notAllowed explains who may take a move, using the business's role names and the field labels. A
// move without its own who words follows the rule for the whole item, so it gets that rule's words.
func (s session) notAllowed(action string, who []string) error {
	if len(who) == 0 {
		return ErrNotYours
	}
	rows, _ := s.tx.Query(s.ctx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key, Name string }])
	if err != nil {
		return fmt.Errorf("workitem: roles: %w", err)
	}
	roleName := func(key string) string {
		for _, r := range list {
			if r.Key == key && r.Name != "" {
				return r.Name
			}
		}
		return strings.ReplaceAll(key, "_", " ")
	}
	fieldName := func(key string) string {
		if f := s.field(key); f != nil && f.Label != "" {
			return strings.ToLower(f.Label)
		}
		return strings.ReplaceAll(key, "_", " ")
	}
	words := engine.DescribeWho(who, roleName, fieldName)
	if slices.Contains(who, engine.WhoHolder) || slices.Contains(who, engine.WhoStarter) {
		words += " (or an admin)"
	}
	return &NotAllowedError{Action: action, Who: words}
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
