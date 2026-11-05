// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations holds the database schema changes, embedded into the binary.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver, which goose uses
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed *.sql
var files embed.FS

// Up applies all pending migrations using an owner connection URL and returns the names of
// the migrations it applied. Notes a migration leaves for whoever runs it (RAISE NOTICE, e.g. how
// many people it could not carry over) are written to notices, if it isn't nil.
func Up(ctx context.Context, databaseURL string, notices io.Writer) ([]string, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if notices != nil {
		cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
			fmt.Fprintln(notices, "note:", n.Message)
		}
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()

	// A database lock makes a second "migrate" (e.g. another test package) wait its turn.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, err
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, r := range results {
		applied = append(applied, r.Source.Path)
	}
	return applied, nil
}
