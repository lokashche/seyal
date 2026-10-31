// SPDX-License-Identifier: AGPL-3.0-or-later

// Package migrations holds the database schema changes, embedded into the binary.
package migrations

import (
	"context"
	"database/sql"
	"embed"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed *.sql
var files embed.FS

// Up applies all pending migrations using an owner connection URL and returns the names of
// the migrations it applied.
func Up(ctx context.Context, databaseURL string) ([]string, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// A database lock makes a second "migrate" (e.g. another test package) wait its turn.
	locker, err := lock.NewPostgresSessionLocker()

	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithSessionLocker(locker))

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
