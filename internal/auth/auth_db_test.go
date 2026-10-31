//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/testdb"
)

const password = "correct horse battery"

func TestLogin(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Login Test")
	in := func(fn func(tx pgx.Tx) error) error { return db.WithTenant(ctx, app, tenant, fn) }
	login := func(email, pw string) (string, auth.Session, error) {
		var token string
		var s auth.Session
		err := in(func(tx pgx.Tx) error {
			var err error
			token, s, err = auth.Login(ctx, tx, email, pw, "test browser")
			return err
		})
		return token, s, err
	}
	check := func(token string) (auth.Session, error) {
		var s auth.Session
		err := in(func(tx pgx.Tx) error {
			var err error
			s, err = auth.Check(ctx, tx, token)
			return err
		})
		return s, err
	}

	if err := in(func(tx pgx.Tx) error {
		return auth.SetPassword(ctx, tx, person, "Sam@Example.test", password, ledger.SystemActor)
	}); err != nil {
		t.Fatal(err)
	}

	token, s, err := login("sam@example.test", password)
	if err != nil || token == "" || s.PersonID != person {
		t.Fatalf("login = %q, %+v, %v", token, s, err)
	}
	if got, err := check(token); err != nil || got.PersonID != person {
		t.Errorf("check = %+v, %v; want the logged-in person", got, err)
	}

	t.Run("wrong password and unknown email look the same", func(t *testing.T) {
		if _, _, err := login("sam@example.test", "wrong password!!"); !errors.Is(err, auth.ErrBadLogin) {
			t.Errorf("wrong password = %v", err)
		}
		if _, _, err := login("nobody@example.test", password); !errors.Is(err, auth.ErrBadLogin) {
			t.Errorf("unknown email = %v", err)
		}
	})

	t.Run("weak passwords are refused", func(t *testing.T) {
		for _, pw := range []string{"short", strings.Repeat("x", 73)} {
			err := in(func(tx pgx.Tx) error {
				return auth.SetPassword(ctx, tx, person, "sam@example.test", pw, ledger.SystemActor)
			})
			if err == nil {
				t.Errorf("password of %d characters accepted", len(pw))
			}
		}
	})

	t.Run("the token itself is not stored", func(t *testing.T) {
		var n int
		if err := in(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE token_sha256 = $1", token).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("found the raw token in the sessions table (%d rows, %v)", n, err)
		}
	})

	t.Run("the ledger never holds the password", func(t *testing.T) {
		var bodies []string
		if err := in(func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, "SELECT body FROM ledger WHERE subject_type = 'person'")
			var err error
			bodies, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, b := range bodies {
			if strings.Contains(b, password) || strings.Contains(b, "$2a$") {
				t.Errorf("ledger event contains the password or its hash: %s", b)
			}
		}
	})

	t.Run("another business can't use the session", func(t *testing.T) {
		other, _ := testdb.NewTenant(t, owner, "Login Test B")
		err := db.WithTenant(ctx, app, other, func(tx pgx.Tx) error {
			_, err := auth.Check(ctx, tx, token)
			return err
		})
		if !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("session in another business = %v, want ErrNoSession", err)
		}
	})

	t.Run("logout ends the session", func(t *testing.T) {
		if err := in(func(tx pgx.Tx) error { return auth.Logout(ctx, tx, token) }); err != nil {
			t.Fatal(err)
		}
		if _, err := check(token); !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("after logout = %v, want ErrNoSession", err)
		}
	})

	t.Run("expired sessions don't work", func(t *testing.T) {
		token, _, err := login("sam@example.test", password)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 minute'")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := check(token); !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("expired session = %v, want ErrNoSession", err)
		}
	})

	t.Run("a disabled person is logged out and can't log in", func(t *testing.T) {
		token, _, err := login("sam@example.test", password)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE persons SET status = 'disabled' WHERE id::text = $1", person)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := check(token); !errors.Is(err, auth.ErrNoSession) {
			t.Errorf("disabled person's session = %v, want ErrNoSession", err)
		}
		if _, _, err := login("sam@example.test", password); !errors.Is(err, auth.ErrBadLogin) {
			t.Errorf("disabled person login = %v, want ErrBadLogin", err)
		}
	})
}
