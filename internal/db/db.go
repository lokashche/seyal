// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db opens database connections and runs work inside scoped transactions: one business, one
// login, or neither.
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

// Scope says what a transaction may see. Row-level security reads it from two settings:
//   - Tenant: one business's rows (app.tenant_id);
//   - Account: one login's own people across its businesses, and only while no business is set
//     (app.account_id; see migration 00015).
//
// An empty field is cleared, so a transaction never keeps a scope it did not ask for.
type Scope struct {
	Tenant  string
	Account string
}

// SetScope sets both scope settings for the rest of tx, in one statement. Open transactions with
// WithTenant, WithAccount or WithServer; call SetScope inside one only to write one login's change
// into each of its businesses in turn.
func SetScope(ctx context.Context, tx pgx.Tx, s Scope) error {
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true), set_config('app.account_id', $2, true)",
		s.Tenant, s.Account)
	return err
}

// WithTenant runs fn in a transaction where row-level security only shows rows of tenantID.
// The transaction commits if fn returns nil and rolls back otherwise.
func WithTenant(ctx context.Context, db Beginner, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("db: empty tenant id")
	}
	return within(ctx, db, Scope{Tenant: tenantID}, fn)
}

// WithAccount runs fn in a transaction with no business set, where the login accountID may read
// its own people (one per business it belongs to) and nothing else of any business.
func WithAccount(ctx context.Context, db Beginner, accountID string, fn func(pgx.Tx) error) error {
	if accountID == "" {
		return errors.New("db: empty account id")
	}
	return within(ctx, db, Scope{Account: accountID}, fn)
}

// WithServer runs fn in a transaction with no business and no login set. It sees no business's
// rows, only server-wide tables: tenants, accounts (without password hashes) and account_sessions.
func WithServer(ctx context.Context, db Beginner, fn func(pgx.Tx) error) error {
	return within(ctx, db, Scope{}, fn)
}

// within runs fn in a transaction with scope s.
func within(ctx context.Context, db Beginner, s Scope, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := SetScope(ctx, tx, s); err != nil {
			return err
		}
		return fn(tx)
	})
}
