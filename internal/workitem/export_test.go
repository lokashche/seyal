// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// This file is compiled only for tests. It lets the tests in package workitem_test reach the
// unexported assign directly, for the kinds of hand-over that have no caller of their own yet (a
// return, until build part 4 adds sending back).

// HandForTest gives the item to person (or to team when person is "") as assign does: by whom, how,
// with which note, and, for a send-back, from whom and after which move.
func HandForTest(ctx context.Context, tx pgx.Tx, id, person, team string, by ledger.Actor, how, note, from, after string) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	err = s.assign(&it, person, team, by, handoff{How: how, Note: note, From: from, After: after})
	return it, err
}
