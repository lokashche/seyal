// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The New mark (D75, M7; Phase 2, build part 5). Each person has their own "last seen" time for each
// item: when they last opened it or did something to it. A card is New when the item came to its
// holder after that. Seen times are kept in item_seen, outside the ledger: they are not work, and only
// the person's own marks use them (nobody else sees them, not even admins).

// MarkSeen records that the person saw the item at the moment at. An older time never replaces a newer
// one. It writes nothing to the ledger and takes no ledger lock, so the caller runs it in a small
// transaction of its own (db.WithTenant) after the page is built.
func MarkSeen(ctx context.Context, tx pgx.Tx, personID, itemID string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO item_seen (tenant_id, person_id, item_id, seen_at) VALUES (current_tenant(), $1, $2, $3)
		ON CONFLICT (tenant_id, person_id, item_id) DO UPDATE SET seen_at = greatest(item_seen.seen_at, excluded.seen_at)`,
		personID, itemID, at.UTC().Truncate(time.Microsecond))
	if err != nil {
		return fmt.Errorf("workitem: mark seen: %w", err)
	}
	return nil
}

// SeenBy returns, for each item the person ever saw, when they last did (HeldSinceLayout). tx must come
// from db.WithTenant.
func SeenBy(ctx context.Context, tx pgx.Tx, personID string) (map[string]string, error) {
	rows, _ := tx.Query(ctx, `SELECT item_id::text, `+stamp("seen_at")+`
		FROM item_seen WHERE tenant_id = current_tenant() AND person_id::text = $1`, personID)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, At string }])
	if err != nil {
		return nil, fmt.Errorf("workitem: seen by: %w", err)
	}
	out := make(map[string]string, len(list))
	for _, s := range list {
		out[s.ID] = s.At
	}
	return out, nil
}

// Arrived is when the item last came to whoever has it now: the later of its latest hand-over and the
// start of its step stay (a new step is new work too, for example for the approvers of an approval
// step). Both are in HeldSinceLayout, so the later one is the larger string. "" for old items that have
// neither.
func (it Item) Arrived() string { return max(it.HeldSince, it.StepSince) }

// IsNew reports whether the item is New for the person: it arrived after they last saw it (seen, ""
// when never). A hand-over they made themselves (for example to their own team) doesn't count; a new
// step stay after it still does, even when they were the one who passed it on: it is someone else's
// step change, such as a submit that now waits for their approval. (When they change the step
// themselves, doing it marks the item seen.) Whether the card is for them at all (one of their own
// folders) is for the caller to say.
func IsNew(it Item, personID, seen string) bool {
	byHandOver := it.HeldSince > seen && it.HandoffFrom != personID
	byStep := it.StepSince > seen && it.StepSince > it.HeldSince
	return byHandOver || byStep
}
