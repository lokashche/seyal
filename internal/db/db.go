// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db opens database connections and runs work inside tenant-scoped transactions.
package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Beginner is anything that can start a transaction: a pool or a single connection.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Open connects to the database at url and checks that the connection works.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// WithTenant runs fn in a transaction where row-level security only shows rows of tenantID.
// The transaction commits if fn returns nil and rolls back otherwise.
func WithTenant(ctx context.Context, db Beginner, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("db: empty tenant id")
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}
