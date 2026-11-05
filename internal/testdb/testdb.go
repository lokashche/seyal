// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testdb prepares the test database for integration tests. Only tests import it.
package testdb

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/config"
	"seyal/internal/db"
	"seyal/migrations"
)

// Connect migrates the test database and returns an owner pool and an app pool. Both are closed
// when the test ends.
func Connect(t testing.TB) (owner, app *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.LoadDotEnv(filepath.Join(root, ".env")); err != nil {
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
	if _, err := migrations.Up(ctx, ownerURL, nil); err != nil {
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

// NewTenant creates a business with a unique slug and one person, and returns both ids.
func NewTenant(t testing.TB, owner *pgxpool.Pool, name string) (tenantID, personID string) {
	t.Helper()
	ctx := context.Background()
	slug := "test-" + strings.ToLower(rand.Text()[:10])
	err := owner.QueryRow(ctx,
		"INSERT INTO tenants (slug, name) VALUES ($1, $2) RETURNING id::text", slug, name).Scan(&tenantID)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	err = db.WithTenant(ctx, owner, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"INSERT INTO persons (tenant_id, display_name) VALUES ($1, 'Test Person') RETURNING id::text",
			tenantID).Scan(&personID)
	})
	if err != nil {
		t.Fatalf("create person: %v", err)
	}
	return tenantID, personID
}

// Email returns a made-up email for name that no other test uses, e.g. sam@t-abcdefghij.test.example.
// A login email is unique across the whole server, and tests share one database (and run at the
// same time), so a fixed address would clash.
func Email(name string) string {
	return name + "@t-" + strings.ToLower(rand.Text()[:10]) + ".test.example"
}

// NewAccount creates a login with a made-up email (see Email) and returns its id and email. Its
// password hash is a placeholder, so nobody can log in with it.
func NewAccount(t testing.TB, owner *pgxpool.Pool) (accountID, email string) {
	t.Helper()
	email = Email("u")
	err := owner.QueryRow(context.Background(),
		"INSERT INTO accounts (email, password_hash) VALUES ($1, 'not a real hash') RETURNING id::text", email).
		Scan(&accountID)
	if err != nil {
		t.Fatalf("create login: %v", err)
	}
	return accountID, email
}

// LinkPerson links a person of a business to a login, and gives the person the login's email.
func LinkPerson(t testing.TB, owner *pgxpool.Pool, tenantID, personID, accountID string) {
	t.Helper()
	ctx := context.Background()
	err := db.WithTenant(ctx, owner, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE persons SET account_id = $1, login_email = (SELECT email FROM accounts WHERE id = $1)
			WHERE id = $2`, accountID, personID)
		return err
	})
	if err != nil {
		t.Fatalf("link person: %v", err)
	}
}

// repoRoot finds the folder that holds go.mod, starting from the current folder.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("testdb: go.mod not found")
		}
		dir = parent
	}
}
