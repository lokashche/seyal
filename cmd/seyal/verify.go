// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/internal/ledger"
)

// runVerify checks the ledger of every business. Normally it continues from the last
// checkpoint; with --full it checks every event from the start.
func runVerify(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(out)
	full := flags.Bool("full", false, "check every event, ignoring earlier checkpoints")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	url, err := config.Need("SEYAL_OWNER_DATABASE_URL")
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	type tenant struct{ ID, Slug string }
	rows, _ := pool.Query(ctx, "SELECT id::text, slug FROM tenants ORDER BY slug")
	tenants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[tenant])
	if err != nil {
		return err
	}

	broken := 0
	for _, t := range tenants {
		var rep ledger.Report
		var tableProblems []audit.Problem
		err := db.WithTenant(ctx, pool, t.ID, func(tx pgx.Tx) error {
			var from ledger.Checkpoint
			if !*full {
				cp, err := ledger.LastCheckpoint(ctx, tx)
				if err != nil {
					return err
				}
				from = cp
			}
			r, err := ledger.Verify(ctx, tx, from)
			if err != nil {
				return err
			}
			rep = r
			if !r.OK() {
				return nil // the tables can only be checked against a sound ledger
			}
			// The tables are always checked in full: they can be edited at any time.
			if tableProblems, err = audit.CheckTables(ctx, tx); err != nil {
				return err
			}
			if len(tableProblems) == 0 {
				return ledger.SaveCheckpoint(ctx, tx, r)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("verify %s: %w", t.Slug, err)
		}
		if rep.OK() && len(tableProblems) == 0 {
			fmt.Fprintf(out, "%s: ok (%d events checked, chain at event %d, tables match)\n",
				t.Slug, rep.Checked, rep.LastSeq)
			continue
		}
		broken++
		fmt.Fprintf(out, "%s: BROKEN\n", t.Slug)
		for _, p := range rep.Problems {
			fmt.Fprintf(out, "  event %d: %s\n", p.Seq, p.Reason)
		}
		for _, p := range tableProblems {
			fmt.Fprintf(out, "  table %s\n", p)
		}
	}
	if broken > 0 {
		return fmt.Errorf("%d of %d businesses have a broken ledger or tables", broken, len(tenants))
	}
	fmt.Fprintf(out, "all %d businesses ok\n", len(tenants))
	return nil
}
