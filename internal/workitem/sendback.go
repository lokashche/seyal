// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
)

// Send back and "returns_to: last" (D68, M4; build part 4).
//
//   - A move marked "back: true" sends work back to an earlier step. So does a refused approval when
//     the process says "on_reject_back: true". Going back is not starting fresh, so the step's "on:"
//     actions don't run again (choice 8).
//   - A step with "returns_to: last" gives work that comes into it again to whoever last had it there:
//     the holder in the snapshot of the latest item.state_changed event that left the step (a person,
//     or a team). That person must still be active and, in a step owned by a role, still have the role.
//     Otherwise the step's owner gets it, as in any step (choice 6).
//   - Every step owner is checked too: nobody gets work who is no longer active. When the owner is gone,
//     work sent back stays with whoever sent it back (or refused it); any other move leaves it where it
//     is. The thread says so (choice 7).

// arrival says how an item came into a step, so the step can work out who gets it.
type arrival struct {
	action string // the move that brought it ("rejected" for a refused approval)
	back   bool   // a rewind: a move marked back, or a refusal with on_reject_back
	from   string // the person who sent it there (pressed the move, or refused); "" for Seyal
	note   string // the reason they gave
}

// dest is who gets an item that enters a step.
type dest struct {
	person, team string
	how          string // HowStep, or HowReturn for work that comes back
	moves        bool   // false: nobody new gets it, so it stays where it is
	// gone says why the step's owner can't have it, e.g. "Asha is no longer active and started this
	// item". Then the item stays where it is, or with whoever sent it back.
	gone string
}

// destination works out who gets the item when it enters state (see the top of this file).
func (s session) destination(it Item, state string, in arrival) (dest, error) {
	kind, name := engine.Owner(s.p, state)
	// A step owned by a person field goes to whoever the field names: someone set it on purpose.
	if kind != "field" && s.p.Steps[state].ReturnsTo == "last" {
		person, team, err := s.lastHolder(it.ID, state)
		if err != nil {
			return dest{}, err
		}
		switch {
		case team != "":
			return dest{team: team, how: HowReturn, moves: true}, nil
		case person != "":
			role := ""
			if kind == "role" {
				role = name
			}
			ok, err := s.mayHold(person, role)
			if err != nil || ok {
				return dest{person: person, how: HowReturn, moves: true}, err
			}
		}
	}
	how := HowStep
	if in.back {
		how = HowReturn
	}
	var person, why string
	switch kind {
	case "role":
		return dest{team: name, how: how, moves: true}, nil
	case "starter":
		person, why = it.CreatedBy, "started this item"
	case "field":
		person, why = fieldPerson(it, name), "is the "+s.fieldLabel(name)
		if person == "" && it.AssigneeID == "" && it.Team == "" {
			// Nobody named yet and nobody has it: it is with whoever started it.
			person, why = it.CreatedBy, "started this item"
		}
	}
	if person == "" {
		return dest{}, nil // no owner here: it stays with whoever had it
	}
	ok, err := s.mayHold(person, "")
	if err != nil || ok {
		return dest{person: person, how: how, moves: true}, err
	}
	d := dest{gone: s.goneWhy(person, why)}
	if in.back && in.from != "" {
		d.person, d.how, d.moves = in.from, HowReturn, true
	}
	return d, nil
}

// handOver gives the item to whoever should have it in its current step (see destination). When the
// step's owner is gone, the thread says where the item stays.
func (s session) handOver(it *Item, in arrival) error {
	d, err := s.destination(*it, it.State, in)
	if err != nil {
		return err
	}
	moved := d.moves && (it.AssigneeID != d.person || it.Team != d.team)
	if d.moves {
		h := handoff{How: d.how}
		if d.how == HowReturn {
			// "From Dev: the March statement is missing": the card names whoever sent it back or refused
			// it, not simply whoever had it last.
			h.Note, h.After, h.From = in.note, in.action, in.from
		}
		if err := s.assign(it, d.person, d.team, ledger.SystemActor, h); err != nil {
			return err
		}
	}
	if d.gone == "" {
		return nil
	}
	return s.postStays(it, d.gone, moved)
}

// postStays says in the thread where the item is now, because the person it should go to is gone: it
// stays with whoever has it, or (moved) it went to whoever sent it back or refused it.
func (s session) postStays(it *Item, why string, moved bool) error {
	roleNames, err := s.roleNames()
	if err != nil {
		return err
	}
	verb := "stays with"
	if moved {
		verb = "goes to"
	}
	text := fmt.Sprintf("%s, so it %s %s. An admin can pass it on.", why, verb, s.holderName(*it, roleNames))
	if it.AssigneeID == "" && it.Team == "" {
		text = why + ", so nobody has it now. An admin can pass it on."
	}
	return s.record("item.posted", *it, map[string]any{"text": text}, ledger.SystemActor)
}

// lastHolder finds who had the item when it last left state: the holder in the snapshot of the
// latest item.state_changed event whose "from" is state. Both are "" when it never left that step (or
// nobody had it then).
func (s session) lastHolder(id, state string) (person, team string, err error) {
	rows, _ := s.tx.Query(s.ctx, `
		SELECT body FROM ledger
		WHERE tenant_id = current_tenant() AND subject_id = $1::uuid AND kind = 'item.state_changed'
		ORDER BY seq DESC`, id)
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", "", fmt.Errorf("workitem: last holder: %w", err)
	}
	for _, body := range bodies {
		var e struct {
			Payload struct {
				Change struct {
					From string `json:"from"`
				} `json:"change"`
				Item Item `json:"item"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return "", "", fmt.Errorf("workitem: last holder: %w", err)
		}
		if e.Payload.Change.From == state {
			return e.Payload.Item.AssigneeID, e.Payload.Item.Team, nil
		}
	}
	return "", "", nil
}

// mayHold reports whether a person may still get work from a step's rule: they are active, and they
// have role (if role isn't "").
func (s session) mayHold(person, role string) (bool, error) {
	var ok bool
	err := s.tx.QueryRow(s.ctx, `
		SELECT p.status = 'active' AND ($2 = '' OR EXISTS (SELECT 1 FROM person_roles r
			WHERE r.tenant_id = p.tenant_id AND r.person_id = p.id AND r.role_key = $2 AND r.revoked_at IS NULL))
		FROM persons p WHERE p.tenant_id = current_tenant() AND p.id::text = $1`, person, role).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workitem: person: %w", err)
	}
	return ok, nil
}

// rejection returns who refused the item's current version, and why.
func (s session) rejection(it Item, r *engine.Approval) (person, reason string, err error) {
	err = s.tx.QueryRow(s.ctx, `
		SELECT coalesce(reason, '') FROM approvals
		WHERE tenant_id = current_tenant() AND work_item_id::text = $1 AND item_version = $2 AND slot = $3`,
		it.ID, r.ItemVersion, r.Slot).Scan(&reason)
	if err != nil {
		return "", "", fmt.Errorf("workitem: refusal: %w", err)
	}
	return r.PersonID, reason, nil
}

// goneWhy says why a person can't get work: "Asha is no longer active and started this item".
func (s session) goneWhy(person, why string) string {
	name, err := s.personName(person)
	if err != nil {
		name = "the person"
	}
	return name + " is no longer active and " + why
}

// fieldLabel is the label people know a field by, or its key.
func (s session) fieldLabel(key string) string {
	if f := s.field(key); f != nil && f.Label != "" {
		return f.Label
	}
	return key
}

// Landing says, before a moves the item with action, where it would go, when that is worth saying
// under the move's button: "Goes back to Asha." for work that returns to whoever last had a step; the
// warning that it will stay with the person sending it, because the step's owner is gone (D68,
// choice 7); or that a team has nobody in it yet (choice 13). It is "" otherwise, also when the move
// isn't possible now. tx must come from db.WithTenant.
func Landing(ctx context.Context, tx pgx.Tx, it Item, action string, a engine.Actor) (string, error) {
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return "", err
	}
	lookup, err := master.LookupFor(ctx, tx, s.p, it.Fields)
	if err != nil {
		return "", err
	}
	line, err := engine.Pick(s.p, it.Engine(), action, lookup)
	if err != nil || line.To == it.State {
		return "", nil // not possible now (the move explains when pressed), or it stays in this step
	}
	d, err := s.destination(it, line.To, arrival{action: action, back: line.Back, from: a.ID})
	if err != nil {
		return "", err
	}
	roleNames, err := s.roleNames()
	if err != nil {
		return "", err
	}
	switch {
	case d.gone != "":
		where := "stay with you"
		switch {
		case d.moves && (it.AssigneeID != d.person || it.Team != ""):
			where = "go to you" // a person who doesn't have it (an admin) sends it back
		case !d.moves && it.AssigneeID != a.ID:
			where = "stay with " + s.holderName(it, roleNames)
		}
		return fmt.Sprintf("%s, so it will %s. An admin can pass it on.", d.gone, where), nil
	case !d.moves:
		return "", nil
	case d.team != "":
		members, err := Members(ctx, tx)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(members, func(m Member) bool { return slices.Contains(m.Roles, d.team) }) {
			return fmt.Sprintf("Nobody is in %s yet. It will wait there until an admin passes it on.", TeamName(d.team, roleNames)), nil
		}
		if d.how == HowReturn {
			return fmt.Sprintf("Goes back to %s.", TeamName(d.team, roleNames)), nil
		}
	case d.how == HowReturn && d.person != a.ID:
		name, err := s.personName(d.person)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Goes back to %s.", name), nil
	}
	return "", nil
}
