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
// the query must find the person events through the ledger_kind index (tenant_id, kind), not by reading
// every event of the business (the ledger only grows). The plan is checked on a copy of the ledger with
// the real indexes and always the same rows (testdb.ShadowLedger), so it doesn't depend on what other
// tests have put in the shared test database meanwhile; plain table reads are turned off too. If no index
// fits, Postgres reads the table or walks the business by its primary key, and the kinds are then only a
// filter, not part of the index look-up.
func TestPeopleNamesUseKindIndex(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, _ := testdb.NewTenant(t, owner, "Names Index Test")
	err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		if err := testdb.ShadowLedger(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, "EXPLAIN "+peopleNamesSQL)
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		looksUpKinds := false
		for _, line := range lines {
			if strings.Contains(line, "Index Cond") && strings.Contains(line, "kind = ANY") {
				looksUpKinds = true
			}
		}
		if !looksUpKinds {
			t.Errorf("the people's names query doesn't look the kinds up in the ledger_kind index:\n%s", strings.Join(lines, "\n"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
