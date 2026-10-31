//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/migrations"
)

// Postgres error code for "insufficient privilege". It covers missing grants, row-level
// security violations and "must be owner".
const codeNoPrivilege = "42501"

func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	owner, app := connect(t)
	a := newTenantWithPerson(t, owner, "Test Business A")
	b := newTenantWithPerson(t, owner, "Test Business B")

	t.Run("a business sees only its own people", func(t *testing.T) {
		err := db.WithTenant(ctx, app, a, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, "SELECT DISTINCT tenant_id::text FROM persons")
			seen, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if len(seen) != 1 || seen[0] != a {
				t.Errorf("business A sees people of %v, want only %s", seen, a)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no business chosen sees nothing", func(t *testing.T) {
		var n int
		if err := app.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("without a business, app sees %d people, want 0", n)
		}
	})

	t.Run("business setting does not leak to the next transaction", func(t *testing.T) {
		conn, err := app.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if err := db.WithTenant(ctx, conn, a, func(pgx.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("after the transaction, same connection sees %d people, want 0", n)
		}
	})

	t.Run("cannot add a person to another business", func(t *testing.T) {
		err := db.WithTenant(ctx, app, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO persons (tenant_id, display_name) VALUES ($1, 'Intruder')", b)
			return err
		})
		wantCode(t, err, codeNoPrivilege)
	})

	t.Run("cannot change another business's people", func(t *testing.T) {
		err := db.WithTenant(ctx, app, a, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "UPDATE persons SET display_name = 'Changed' WHERE tenant_id = $1", b)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("business A changed %d people of business B, want 0", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cannot delete", func(t *testing.T) {
		err := db.WithTenant(ctx, app, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "DELETE FROM persons")
			return err
		})
		wantCode(t, err, codeNoPrivilege)
	})

	t.Run("cannot turn the lock off", func(t *testing.T) {
		_, err := app.Exec(ctx, "ALTER TABLE persons DISABLE ROW LEVEL SECURITY")
		wantCode(t, err, codeNoPrivilege)
	})

	t.Run("cannot create a business", func(t *testing.T) {
		_, err := app.Exec(ctx, "INSERT INTO tenants (slug, name) VALUES ('sneaky', 'Sneaky')")
		wantCode(t, err, codeNoPrivilege)
	})

	t.Run("empty business id is refused", func(t *testing.T) {
		if err := db.WithTenant(ctx, app, "", func(pgx.Tx) error { return nil }); err == nil {
			t.Error("WithTenant accepted an empty tenant id")
		}
	})
}

// connect migrates the test database and opens an owner pool and an app pool.
func connect(t *testing.T) (owner, app *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if err := config.LoadDotEnv("../../.env"); err != nil {
		t.Fatal(err)
	}
	ownerURL, err := config.Need("SEYAL_TEST_OWNER_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	appURL, err := config.Need("SEYAL_TEST_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Up(ctx, ownerURL); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if owner, err = db.Open(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if app, err = db.Open(ctx, appURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return owner, app
}

// newTenantWithPerson creates a business with a unique slug and one person, and returns its id.
func newTenantWithPerson(t *testing.T, owner *pgxpool.Pool, name string) string {
	t.Helper()
	ctx := context.Background()
	slug := "test-" + strings.ToLower(rand.Text()[:10])
	var id string
	err := owner.QueryRow(ctx,
		"INSERT INTO tenants (slug, name) VALUES ($1, $2) RETURNING id::text", slug, name).Scan(&id)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	err = db.WithTenant(ctx, owner, id, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO persons (tenant_id, display_name) VALUES ($1, 'Test Person')", id)
		return err
	})
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	return id
}

// wantCode fails the test unless err is a Postgres error with the given code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("got error %v, want Postgres error code %s", err, code)
	}
}
