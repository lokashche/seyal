// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testdb prepares the test database for integration tests. Only tests import it.
package testdb

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
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
	if owner, err = db.Open(ctx, lockLimit(ownerURL)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if app, err = db.Open(ctx, lockLimit(appURL)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return owner, app
}

// lockLimit adds a time limit for waiting on a lock (lock_timeout) to a test database address. All
// test packages share one database at the same time, so a test that waits on a lock held by another
// package's stuck transaction would otherwise wait until go test gives up after 10 minutes, with no
// word on why. With the limit, the statement fails after a minute and Postgres's error names the wait.
// An address that isn't a postgres:// URL is left as it is.
func lockLimit(address string) string {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return address
	}
	q := u.Query()
	if q.Get("lock_timeout") == "" {
		q.Set("lock_timeout", "60s") // pgx passes settings it doesn't know on to Postgres
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// NewTenant creates a business with a unique slug and one person, and returns both ids. The person
// has no login; GiveLogins gives them one.
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

// GiveLogins gives each person a made-up login (see NewAccount), so work can go to them: Seyal gives
// work only to people with a login in the business (D68, choice 12; D73).
func GiveLogins(t testing.TB, owner *pgxpool.Pool, tenantID string, personIDs ...string) {
	t.Helper()
	for _, id := range personIDs {
		account, _ := NewAccount(t, owner)
		LinkPerson(t, owner, tenantID, id, account)
	}
}

// ShadowLedger gives tx, until it ends, its own copy of the ledger table: the same columns and
// indexes, filled with the same made-up rows every time (50 businesses, tx's own among them, with 400
// events each, 40 kinds and 50 subjects per business), with its statistics read. A temporary table
// comes first when Postgres looks up a table name, so every query in tx that names "ledger" then reads
// the copy. tx must come from db.WithTenant.
//
// It is for checking a query plan (EXPLAIN) only. Postgres picks a plan from how many rows it expects,
// and it reads the business id from app.tenant_id while planning: a brand-new test business has
// almost no events, and how many Postgres guesses for it depends on the whole shared test database,
// which other packages fill while the tests run. With a guess of one or two rows, walking the whole
// business by its primary key looks as cheap as any index, so a plan check could pass alone and fail
// in the full run. In the copy, the business always has 400 events among 50 businesses, so the plan is
// the same in every run.
func ShadowLedger(ctx context.Context, tx pgx.Tx) error {
	for _, sql := range []string{
		`CREATE TEMPORARY TABLE ledger (LIKE public.ledger INCLUDING DEFAULTS INCLUDING INDEXES) ON COMMIT DROP`,
		`INSERT INTO pg_temp.ledger (tenant_id, seq, occurred_at, kind, subject_type, subject_id, actor_kind, body, prev_hash, hash)
		SELECT CASE WHEN b = 1 THEN current_tenant() ELSE md5('business ' || b)::uuid END, s, now(),
			coalesce((ARRAY['person.created', 'person.renamed', 'person.updated'])[s % 40 + 1], 'test.kind' || s % 40),
			'work_item', md5('subject ' || b || ' ' || s % 50)::uuid, 'system', '{}',
			decode(repeat('00', 32), 'hex'), decode(repeat('00', 32), 'hex')
		FROM generate_series(1, 50) b, generate_series(1, 400) s`,
		`ANALYZE pg_temp.ledger`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return nil
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
