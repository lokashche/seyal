// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Moving someone's open work in one go (D75, M3), for example while they are on leave. An admin asks on
// Tell Seyal ("Ben is on leave, move his work to Dev"); Seyal lists Ben's open items, and nothing moves
// until the admin presses the button. Reassign then does each item as an admin's Pass on would, with the
// same checks and notices, recorded as the admin (how: reassign).
//
//   - On the press everything is checked again: the person pressing is still an admin, the receiver
//     may still receive work, and each item is still with the person the list showed. Without admin
//     rights nothing moves.
//   - Each item runs in a savepoint (a mark inside the transaction that its changes can be undone to):
//     an item that can't move is undone alone, named, and the others still move.
//   - One random batch id, from crypto/rand, is kept on every move of one press, so they can be found
//     together later (a "Move back" button, say).

// ErrNotAdmin means the person may not move someone else's work: only admins may (D75, decision 18).
var ErrNotAdmin = refusal("only an admin can move someone else's work. You can pass your own items one by one")

// MaxMove is the most items one press may move. Each item is checked and moved while the business's
// ledger is locked, so a press with a huge list (a broken page, or a forged form) can't hold up
// everyone else's work for long.
const MaxMove = 500

// ErrTooMany means one press named more than MaxMove items.
var ErrTooMany = fmt.Errorf("that is more than %d items in one go; please pick fewer", MaxMove)

// Reassigned is what one press did.
type Reassigned struct {
	Batch   string
	Moved   []Item
	Notices []string  // what an admin's Pass on says, per item that moved: "W-2026-0004: Passed to ..."
	Skipped []Skipped // items that didn't move, and why
}

// Skipped is an item that didn't move.
type Skipped struct{ Number, Why string }

// Reassign moves the items with the ids from person from to person to, as by (an admin), with the note
// (empty: "Moved here from <from>"). Items no longer with from are skipped and named. Nothing moves when
// by isn't an admin now, to may not receive work, or ids has more than MaxMove items (an id named
// twice counts once). tx must come from db.WithTenant.
func Reassign(ctx context.Context, tx pgx.Tx, ids []string, from, to, note string, by ledger.Actor) (Reassigned, error) {
	// An id named twice is moved once. The count is checked before the lock is taken.
	seen := map[string]bool{}
	ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
		dup := seen[id]
		seen[id] = true
		return dup
	})
	if len(ids) > MaxMove {
		return Reassigned{}, ErrTooMany
	}
	// The ledger lock is taken here, outside the savepoints: a lock taken inside one would be let go
	// if that savepoint is undone.
	if err := ledger.Lock(ctx, tx); err != nil {
		return Reassigned{}, err
	}
	s := session{ctx: ctx, tx: tx}
	a, err := s.actor(by)
	if err != nil {
		return Reassigned{}, err
	}
	if !a.Admin || by.Kind != ledger.Human {
		return Reassigned{}, ErrNotAdmin
	}
	if err := MayMove(ctx, tx, from, to); err != nil {
		return Reassigned{}, err
	}
	fromName, err := s.personName(from)
	if err != nil {
		return Reassigned{}, err
	}
	if strings.TrimSpace(note) == "" {
		note = "Moved here from " + fromName
	}
	out := Reassigned{Batch: rand.Text()}
	for _, id := range ids {
		sp, err := tx.Begin(ctx) // a savepoint
		if err != nil {
			return Reassigned{}, err
		}
		it, notice, why, err := reassignOne(ctx, sp, id, from, to, note, out.Batch, by)
		if err != nil {
			sp.Rollback(ctx)
			return Reassigned{}, err
		}
		if why.Why != "" {
			if err := sp.Rollback(ctx); err != nil {
				return Reassigned{}, err
			}
			out.Skipped = append(out.Skipped, why)
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return Reassigned{}, err
		}
		out.Moved = append(out.Moved, it)
		if notice != "" {
			out.Notices = append(out.Notices, it.Number+": "+notice)
		}
	}
	return out, nil
}

// reassignOne moves one item, or says why it didn't (skip). err is only for a fault (the database),
// which stops the whole press.
func reassignOne(ctx context.Context, tx pgx.Tx, id, from, to, note, batch string, by ledger.Actor) (it Item, notice string, skip Skipped, err error) {
	it, err = Get(ctx, tx, id)
	if errors.Is(err, ErrNotFound) {
		return Item{}, "", Skipped{Number: "An item", Why: "it isn't in this business"}, nil
	}
	if err != nil {
		return Item{}, "", Skipped{}, err
	}
	skip.Number = it.Number
	if it.HolderKey() != "person:"+from {
		s, err := open(ctx, tx, it.PlaybookVersionID)
		if err != nil {
			return Item{}, "", Skipped{}, err
		}
		roles, err := s.roleNames()
		if err != nil {
			return Item{}, "", Skipped{}, err
		}
		skip.Why = "it is with " + s.holderName(it, roles) + " now"
		return Item{}, "", skip, nil
	}
	it, notice, err = Pass(ctx, tx, id, PassOn{Person: to, Note: note, Via: "button", Seen: "person:" + from,
		how: HowReassign, batch: batch}, by)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) || ctx.Err() != nil:
		return Item{}, "", Skipped{}, err
	case err != nil:
		skip.Why = err.Error()
		return Item{}, "", skip, nil
	}
	return it, notice, Skipped{}, nil
}

// MayMove says why from's open work can't go to to: to is from, or may not receive work (no longer
// active, no login, not in this business). It is nil when it may. tx must come from db.WithTenant.
func MayMove(ctx context.Context, tx pgx.Tx, from, to string) error {
	s := session{ctx: ctx, tx: tx}
	if from == to {
		name, err := s.personName(from)
		if err != nil {
			return err
		}
		return fmt.Errorf("that is %s again; choose someone else to move the work to", name)
	}
	members, err := Members(ctx, tx)
	if err != nil {
		return err
	}
	return s.mayReceive(to, members)
}

// OpenWorkOf returns the open items assigned to the person themselves, by number: not the items of
// their team, not the approvals waiting on their role, and not the items they only started (D75, M3).
// tx must come from db.WithTenant.
func OpenWorkOf(ctx context.Context, tx pgx.Tx, person string) ([]Item, error) {
	items, err := List(ctx, tx)
	if err != nil {
		return nil, err
	}
	items = slices.DeleteFunc(items, func(it Item) bool { return it.AssigneeID != person })
	slices.SortFunc(items, func(x, y Item) int { return strings.Compare(x.Number, y.Number) })
	return items, nil
}

// FormerRoles returns, per person id, the roles they had and lost (taken away in Settings). tx must
// come from db.WithTenant.
func FormerRoles(ctx context.Context, tx pgx.Tx) (map[string][]string, error) {
	rows, _ := tx.Query(ctx, `
		SELECT person_id::text, role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND revoked_at IS NOT NULL`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Person, Role string }])
	if err != nil {
		return nil, fmt.Errorf("workitem: former roles: %w", err)
	}
	out := map[string][]string{}
	for _, r := range list {
		out[r.Person] = append(out[r.Person], r.Role)
	}
	return out, nil
}

// Stranded says why the person who has the item can't work it any more, so an admin should pass it on
// (D75, M3): gone is true when they are no longer active or have no login (they aren't in members);
// role is the key of the step's role when they lost it while having the item (lost: FormerRoles). Both
// are empty when a team or nobody has it, or when the person can still work it. A person an admin
// passed the work to on purpose, who never had the role, isn't stranded: they see "pass it on"
// themselves.
func Stranded(p *playbook.Playbook, it Item, members []Member, lost map[string][]string) (gone bool, role string) {
	if it.AssigneeID == "" {
		return false, ""
	}
	i := slices.IndexFunc(members, func(m Member) bool { return m.ID == it.AssigneeID })
	if i < 0 {
		return true, ""
	}
	if kind, step := engine.Owner(p, it.State); kind == "role" && !slices.Contains(members[i].Roles, step) &&
		slices.Contains(lost[it.AssigneeID], step) {
		return false, step
	}
	return false, ""
}

// WorkedEscalated returns the steps (state keys) that person worked themselves after Seyal had
// escalated them (D75, decision 11): they changed the details, or took a move out of the step, in a
// step stay that Seyal had handed on, while not in the step's own role (an admin from the Owner team,
// say). Taking it, passing it on and writing notes don't count, and nor does someone of the step's role
// finishing it after it was passed back. The approval's ledger event says so, because the person may
// now be checking their own work. Whether they were in the step's role is judged at the moment of each
// change, from the role changes in the ledger, so a role given or taken later doesn't change the
// answer. tx must come from db.WithTenant.
func WorkedEscalated(ctx context.Context, tx pgx.Tx, itemID, person string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT seq, kind, coalesce(actor_person_id::text, ''), coalesce(on_behalf_of::text, ''), body FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
		ORDER BY seq`, itemID)
	if err != nil {
		return nil, fmt.Errorf("workitem: escalated work: %w", err)
	}
	defer rows.Close()
	type work struct {
		step string
		seq  int64
	}
	var done []work             // the person's changes in an escalated step stay
	var before Item             // the item as it was before the event being read
	var escStay, escStep string // the step stay Seyal handed on, and its step
	for rows.Next() {
		var seq int64
		var kind, actor, behalf, body string
		if err := rows.Scan(&seq, &kind, &actor, &behalf, &body); err != nil {
			return nil, fmt.Errorf("workitem: escalated work: %w", err)
		}
		var e struct {
			Payload struct {
				Change map[string]any `json:"change"`
				Item   Item           `json:"item"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("workitem: escalated work: %w", err)
		}
		by := cmp.Or(actor, behalf)
		if escStay != "" && before.StepSince == escStay && by == person &&
			(kind == "item.fields_set" || kind == "item.state_changed") {
			done = append(done, work{escStep, seq})
		}
		if how, _ := e.Payload.Change["how"].(string); kind == "item.assigned" && how == HowEscalate {
			escStay, escStep = e.Payload.Item.StepSince, e.Payload.Item.State
		}
		before = e.Payload.Item
	}
	if err := rows.Err(); err != nil || len(done) == 0 {
		return nil, err
	}
	s, err := open(ctx, tx, before.PlaybookVersionID)
	if err != nil {
		return nil, err
	}
	heldAt, err := rolesAt(ctx, tx, person)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, w := range done {
		// Only steps outside the roles the person held when they did the work.
		if kind, role := engine.Owner(s.p, w.step); kind == "role" && slices.Contains(heldAt(w.seq), role) {
			continue
		}
		if !slices.Contains(out, w.step) {
			out = append(out, w.step)
		}
	}
	return out, nil
}

// rolesAt returns a func that says which roles person held just after the ledger event seq: their
// roles now, with each later role.granted taken off and each later role.revoked put back.
func rolesAt(ctx context.Context, tx pgx.Tx, person string) (func(seq int64) []string, error) {
	rows, _ := tx.Query(ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL`, person)
	now, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("workitem: roles: %w", err)
	}
	rows, _ = tx.Query(ctx, `
		SELECT seq, kind, coalesce(body::jsonb #>> '{payload,role}', '') FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'person' AND subject_id = $1::uuid
		  AND kind IN ('role.granted', 'role.revoked')
		ORDER BY seq DESC`, person)
	changes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Seq        int64
		Kind, Role string
	}])
	if err != nil {
		return nil, fmt.Errorf("workitem: role changes: %w", err)
	}
	return func(seq int64) []string {
		held := slices.Clone(now)
		for _, c := range changes {
			if c.Seq <= seq {
				break
			}
			if c.Kind == "role.granted" {
				held = slices.DeleteFunc(held, func(r string) bool { return r == c.Role })
			} else if !slices.Contains(held, c.Role) {
				held = append(held, c.Role)
			}
		}
		return held
	}, nil
}

// WorkedOnApprovals returns, per approval slot and approver ("owner person-id"), the steps the
// approval's own ledger event recorded as worked after an escalation (WorkedEscalated, when the
// approval was given). The approval card shows these, so the card and the ledger always agree. tx must
// come from db.WithTenant.
func WorkedOnApprovals(ctx context.Context, tx pgx.Tx, itemID string) (map[string][]string, error) {
	rows, _ := tx.Query(ctx, `
		SELECT coalesce(body::jsonb #>> '{payload,change,slot}', '') || ' ' || coalesce(actor_person_id::text, ''),
		       coalesce(body::jsonb #> '{payload,change,worked_escalated}', '[]'::jsonb)
		FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
		  AND kind = 'item.decided' AND body::jsonb #>> '{payload,change,decision}' = $2
		ORDER BY seq`, itemID, engine.Approve)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Key   string
		Steps []string
	}])
	if err != nil {
		return nil, fmt.Errorf("workitem: approvals' escalated work: %w", err)
	}
	out := map[string][]string{}
	for _, r := range list {
		out[r.Key] = r.Steps // the latest approval of that slot by that person wins
	}
	return out, nil
}

// WorkedEscalatedLine says it in words: "Chitra worked With ops after it was escalated", or "".
func WorkedEscalatedLine(name string, steps []string) string {
	if len(steps) == 0 {
		return ""
	}
	names := make([]string, len(steps))
	for i, st := range steps {
		names[i] = stepName(st)
	}
	return name + " worked " + andList(names) + " after it was escalated"
}
