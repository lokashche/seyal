//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/testdb"
)

// TestPeopleNamesUseKindIndex: the names people have had are read each time the AI reads a message, so
// the query must find the person events through the ledger_kind index, not by reading every event of
// the business (the ledger only grows). Plain table reads are turned off for the check, so the test does
// not depend on how many rows the test database happens to hold: if no index fits, Postgres still reads
// the table, or walks the whole business by its primary key, and the plan does not name ledger_kind.
func TestPeopleNamesUseKindIndex(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, _ := testdb.NewTenant(t, owner, "Names Index Test")
	err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, "EXPLAIN "+peopleNamesSQL)
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if plan := strings.Join(lines, "\n"); !strings.Contains(plan, "ledger_kind") {
			t.Errorf("the people's names query doesn't use the ledger_kind index:\n%s", plan)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
