//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/db"
	"seyal/internal/testdb"
)

// Postgres error codes: RAISE EXCEPTION without a code of its own (used by our triggers), and no
// such function.
const (
	codeRaised     = "P0001"
	codeNoFunction = "42883"
)

// TestAccounts: logins are server-wide (no row-level security), but the app can't read password
// hashes (it can only ask account_check whether a password is right) or change emails, and a login
// sees its own people only while no business is set (D64).
func TestAccounts(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	a, aEmail := testdb.NewAccount(t, owner)
	b, _ := testdb.NewAccount(t, owner)
	t1, p1 := testdb.NewTenant(t, owner, "Accounts Test One")
	t2, p2 := testdb.NewTenant(t, owner, "Accounts Test Two")
	t3, p3 := testdb.NewTenant(t, owner, "Accounts Test Three")
	testdb.LinkPerson(t, owner, t1, p1, a)
	testdb.LinkPerson(t, owner, t2, p2, a)
	testdb.LinkPerson(t, owner, t3, p3, b)
	unlinked := addPerson(t, owner, t1, "Not Linked", nil, nil, false)

	t.Run("the app can't read password hashes", func(t *testing.T) {
		for _, q := range []string{"SELECT password_hash FROM accounts", "SELECT * FROM accounts"} {
			_, err := app.Exec(ctx, q)
			wantCode(t, err, codeNoPrivilege)
		}
		var email string
		err := db.WithServer(ctx, app, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT email FROM accounts WHERE id = $1", a).Scan(&email)
		})
		if err != nil || email != aEmail {
			t.Errorf("reading id and email = %q, %v; want %q", email, err, aEmail)
		}
	})

	t.Run("no function gives the app a password hash", func(t *testing.T) {
		// The query a reviewer found against the old account_password(): list the emails, ask for each
		// hash. Now the only function the app may run as the owner is account_check, which answers with
		// a login, a flag and yes or no, never a hash.
		rows, _ := app.Query(ctx, `SELECT p.proname || '(' || pg_get_function_arguments(p.oid) || ') returns ' || pg_get_function_result(p.oid)
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.prosecdef AND has_function_privilege('seyal_app', p.oid, 'EXECUTE')`)
		definers, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		want := "account_check(e text, pw text) returns TABLE(id uuid, password_temporary boolean, ok boolean)"
		if !slices.Equal(definers, []string{want}) {
			t.Errorf("functions that run as the owner for the app = %q, want only %q", definers, want)
		}
		_, err = app.Exec(ctx, "SELECT * FROM account_password($1)", aEmail)
		wantCode(t, err, codeNoFunction)
	})

	t.Run("account_check tells right from wrong", func(t *testing.T) {
		// Real Go bcrypt hashes (golang.org/x/crypto/bcrypt, cost 10), made once for this test.
		const (
			horse = "$2a$10$jFhNh0t6kYjewLuFwhqV0egalDcSIB9GsiJFVGxMIx9irjhyJoEvu" // "correct horse"
			long  = "$2a$10$DXFWoBLdKzc3oat7TvOdquJYyQEh0ZNdtv3Q.iwwiBml21b80618u" // 72 times "a"
		)
		withHash := func(hash string, temporary bool) (id, email string) {
			t.Helper()
			id, email = testdb.NewAccount(t, owner)
			if _, err := owner.Exec(ctx, "UPDATE accounts SET password_hash = $1, password_temporary = $2 WHERE id = $3", hash, temporary, id); err != nil {
				t.Fatal(err)
			}
			return id, email
		}
		h, hEmail := withHash(horse, true)
		l, lEmail := withHash(long, false)
		check := func(email, pw string) (rows int, id string, temporary, ok bool) {
			t.Helper()
			r, err := app.Query(ctx, "SELECT id::text, password_temporary, ok FROM account_check($1, $2)", email, pw)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			for r.Next() {
				rows++
				if err := r.Scan(&id, &temporary, &ok); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Err(); err != nil {
				t.Fatal(err)
			}
			return rows, id, temporary, ok
		}
		for _, c := range []struct {
			name, email, pw string
			id              string // "" for no row
			temporary, ok   bool
		}{
			{"right password", hEmail, "correct horse", h, true, true},
			{"email in capitals", strings.ToUpper(hEmail), "correct horse", h, true, true},
			{"wrong password", hEmail, "correct hors", h, true, false},
			{"empty password", hEmail, "", h, true, false},
			{"72 bytes", lEmail, strings.Repeat("a", 72), l, false, true},
			{"71 of the 72 bytes", lEmail, strings.Repeat("a", 71), l, false, false},
			// Like Go's bcrypt, only the first 72 bytes count (Go refuses to make a longer password).
			{"73 bytes, the first 72 right", lEmail, strings.Repeat("a", 72) + "b", l, false, true},
			{"a placeholder hash never matches", aEmail, "not a real hash", a, false, false},
			{"unknown email", testdb.Email("nobody"), "correct horse", "", false, false},
		} {
			rows, id, temporary, ok := check(c.email, c.pw)
			want := 1
			if c.id == "" {
				want = 0
			}
			if rows != want || id != c.id || temporary != c.temporary || ok != c.ok {
				t.Errorf("%s: %d rows, login %q, temporary %v, ok %v; want %d, %q, %v, %v",
					c.name, rows, id, temporary, ok, want, c.id, c.temporary, c.ok)
			}
		}
	})

	t.Run("passwords stay out of the server log", func(t *testing.T) {
		// account_check's password is a query parameter. Under these settings (Postgres's defaults)
		// no statement is logged, and a failed one is logged without its parameter values.
		for name, want := range map[string]string{
			"log_statement":                     "none",
			"log_min_duration_statement":        "-1",
			"log_parameter_max_length_on_error": "0",
		} {
			var got string
			if err := app.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&got); err != nil || got != want {
				t.Errorf("%s = %q (%v), want %q", name, got, err, want)
			}
		}
	})

	t.Run("the app can't change an email, delete logins or run the carry-over", func(t *testing.T) {
		for _, q := range []string{
			"UPDATE accounts SET email = 'x' || email WHERE id = '" + a + "'",
			"DELETE FROM accounts WHERE id = '" + a + "'",
			"DELETE FROM account_sessions",
			"UPDATE account_sessions SET account_id = '" + b + "'",
			"SELECT link_existing_logins('" + t1 + "')",
		} {
			_, err := app.Exec(ctx, q)
			wantCode(t, err, codeNoPrivilege)
		}
		// Only the columns a new login needs: never its id.
		_, err := app.Exec(ctx, "INSERT INTO accounts (id, email, password_hash) VALUES (gen_random_uuid(), $1, 'x')",
			testdb.Email("chosen-id"))
		wantCode(t, err, codeNoPrivilege)
	})

	t.Run("the app can add a login, set its password and keep its sessions", func(t *testing.T) {
		err := db.WithServer(ctx, app, func(tx pgx.Tx) error {
			var id string
			if err := tx.QueryRow(ctx, "INSERT INTO accounts (email, password_hash, password_temporary) VALUES ($1, 'first', true) RETURNING id::text",
				testdb.Email("new")).Scan(&id); err != nil {
				return fmt.Errorf("add a login: %w", err)
			}
			if _, err := tx.Exec(ctx, "UPDATE accounts SET password_hash = 'second', password_temporary = false WHERE id = $1", id); err != nil {
				return fmt.Errorf("set its password: %w", err)
			}
			token := fmt.Sprintf("%x", sha256.Sum256([]byte(id))) // a made-up token's SHA-256, unique to this login
			if _, err := tx.Exec(ctx, "INSERT INTO account_sessions (token_sha256, account_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')",
				token, id); err != nil {
				return fmt.Errorf("start a session: %w", err)
			}
			var owner string
			if err := tx.QueryRow(ctx, "SELECT account_id::text FROM account_sessions WHERE token_sha256 = $1", token).Scan(&owner); err != nil || owner != id {
				return fmt.Errorf("read the session = %q, %v; want login %s", owner, err, id)
			}
			tag, err := tx.Exec(ctx, "UPDATE account_sessions SET ended_at = now() WHERE token_sha256 = $1 AND ended_at IS NULL", token)
			if err != nil || tag.RowsAffected() != 1 {
				return fmt.Errorf("end the session = %d rows, %v; want 1", tag.RowsAffected(), err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})

	t.Run("the app changes only a person's name, email, status and login (00016)", func(t *testing.T) {
		for _, set := range []string{"password_hash = NULL", "password_temporary = true", "tenant_id = tenant_id", "id = id", "created_at = now()"} {
			err := db.WithTenant(ctx, app, t1, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "UPDATE persons SET "+set+" WHERE id = $1", unlinked)
				return err
			})
			wantCode(t, err, codeNoPrivilege)
		}
		err := db.WithTenant(ctx, app, t1, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE persons SET display_name = display_name, login_email = login_email,
				status = status, account_id = account_id WHERE id = $1`, unlinked)
			return err
		})
		if err != nil {
			t.Errorf("changing name, email, status and login = %v", err)
		}
	})

	t.Run("a login sees exactly its own people, and only with no business set", func(t *testing.T) {
		if got := visiblePeople(t, app, db.Scope{Account: a}); !sameSet(got, p1, p2) {
			t.Errorf("login A sees %v, want its two people %s and %s", got, p1, p2)
		}
		if got := visiblePeople(t, app, db.Scope{Account: b}); !sameSet(got, p3) {
			t.Errorf("login B sees %v, want only %s", got, p3)
		}
		// Inside a business, a leftover login setting adds nothing: only that business's people.
		if got := visiblePeople(t, app, db.Scope{Tenant: t1, Account: a}); !sameSet(got, p1, unlinked) {
			t.Errorf("business 1 with login A set sees %v, want only its own %s and %s", got, p1, unlinked)
		}
		if got := visiblePeople(t, app, db.Scope{}); len(got) != 0 {
			t.Errorf("with no scope, the app sees %d people, want 0", len(got))
		}
		var n int
		if err := app.QueryRow(ctx, "SELECT count(*) FROM persons").Scan(&n); err != nil || n != 0 {
			t.Errorf("outside any transaction helper, the app sees %d people (%v), want 0", n, err)
		}
	})

	t.Run("a login can't add or change people", func(t *testing.T) {
		err := db.WithAccount(ctx, app, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO persons (tenant_id, display_name, account_id) VALUES ($1, 'Sneaky', $2)", t3, a)
			return err
		})
		wantCode(t, err, codeNoPrivilege)
		err = db.WithAccount(ctx, app, a, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "UPDATE persons SET display_name = 'Changed' WHERE account_id = $1", a)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("login A changed %d of its people, want 0", tag.RowsAffected())
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("scope settings don't leak to the next transaction", func(t *testing.T) {
		conn, err := app.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		// Checked after each helper on its own, so neither setting can hide behind the next one.
		after := func(name string) {
			t.Helper()
			var n int
			var tenant, account string
			if err := conn.QueryRow(ctx, `SELECT count(*), coalesce(current_setting('app.tenant_id', true), ''),
				coalesce(current_setting('app.account_id', true), '') FROM persons`).Scan(&n, &tenant, &account); err != nil {
				t.Fatal(err)
			}
			if n != 0 || tenant != "" || account != "" {
				t.Errorf("after %s, the connection sees %d people, business %q and login %q; want 0 and none", name, n, tenant, account)
			}
		}
		if err := db.WithAccount(ctx, conn, a, func(pgx.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		after("WithAccount")
		if err := db.WithTenant(ctx, conn, t1, func(pgx.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		after("WithTenant")
	})

	t.Run("a business's transaction clears a stray login", func(t *testing.T) {
		conn, err := app.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		// A login left on the connection by mistake (a session-wide setting, not one transaction's).
		if _, err := conn.Exec(ctx, "SELECT set_config('app.account_id', $1, false)", a); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(ctx, "RESET app.account_id")
		var account string
		var people int
		err = db.WithTenant(ctx, conn, t2, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT coalesce(current_account()::text, ''), count(*) FROM persons").Scan(&account, &people)
		})
		if err != nil || account != "" || people != 1 {
			t.Errorf("WithTenant saw login %q and %d people (%v), want no login and business 2's one person", account, people, err)
		}
	})

	t.Run("empty login id is refused", func(t *testing.T) {
		if err := db.WithAccount(ctx, app, "", func(pgx.Tx) error { return nil }); err == nil {
			t.Error("WithAccount accepted an empty account id")
		}
	})

	t.Run("a person's login is removed or set, never moved", func(t *testing.T) {
		p := addPerson(t, owner, t1, "Moving Person", nil, nil, false)
		set := func(account any) error {
			return db.WithTenant(ctx, app, t1, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "UPDATE persons SET account_id = $1 WHERE id = $2", account, p)
				return err
			})
		}
		c, _ := testdb.NewAccount(t, owner)
		if err := set(c); err != nil {
			t.Fatalf("linking = %v", err)
		}
		wantCode(t, set(b), codeRaised)
		if err := set(nil); err != nil {
			t.Errorf("removing the login = %v", err)
		}
		if err := set(b); err != nil {
			t.Errorf("linking another login after removing = %v", err)
		}
	})

	t.Run("one person per login in each business", func(t *testing.T) {
		err := db.WithTenant(ctx, app, t1, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE persons SET account_id = $1 WHERE id = $2", a, unlinked)
			return err
		})
		wantCode(t, err, "23505") // unique violation
	})
}

// TestEveryBusinessTableIsLocked: every table has row-level security switched on and forced, except
// the few that are server-wide by design.
func TestEveryBusinessTableIsLocked(t *testing.T) {
	_, app := testdb.Connect(t)
	serverWide := []string{"tenants", "accounts", "account_sessions", "goose_db_version"}
	rows, err := app.Query(context.Background(), `SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			t.Fatal(err)
		}
		seen++
		if !slices.Contains(serverWide, name) && (!enabled || !forced) {
			t.Errorf("table %s: row-level security enabled=%v forced=%v, want both", name, enabled, forced)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen < 10 {
		t.Errorf("found only %d tables; is this the right database?", seen)
	}
}

// TestCarryOverLogins: migration 00015 gives each person with a password a login with the same email
// and password. Here the owner runs it on fresh businesses, the way the migration does.
func TestCarryOverLogins(t *testing.T) {
	ctx := context.Background()
	owner, _ := testdb.Connect(t)
	email := testdb.Email("old")
	t1, _ := testdb.NewTenant(t, owner, "Carry-over One")
	t2, _ := testdb.NewTenant(t, owner, "Carry-over Two")
	t3, _ := testdb.NewTenant(t, owner, "Carry-over Three")
	p1 := addPerson(t, owner, t1, "Old Login", email, "hash-one", true)
	p2 := addPerson(t, owner, t2, "Old Login", email, "hash-two", false)
	p3 := addPerson(t, owner, t3, "Old Login", email, "hash-one", true) // a copy of the same password

	run := func(tenant string) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(ctx, "SELECT link_existing_logins($1)", tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	type login struct {
		id, hash  string
		temporary bool
	}
	logins := func() []login {
		t.Helper()
		rows, _ := owner.Query(ctx, "SELECT id::text, password_hash, password_temporary FROM accounts WHERE email = $1", email)
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (login, error) {
			var l login
			return l, r.Scan(&l.id, &l.hash, &l.temporary)
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	linkOf := func(tenant, person string) (account, hash string) {
		t.Helper()
		err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT coalesce(account_id::text, ''), password_hash FROM persons WHERE id = $1", person).
				Scan(&account, &hash)
		})
		if err != nil {
			t.Fatal(err)
		}
		return account, hash
	}

	if n := run(t1); n != 0 {
		t.Errorf("first business: %d left unlinked, want 0", n)
	}
	got := logins()
	if len(got) != 1 || got[0].hash != "hash-one" || !got[0].temporary {
		t.Fatalf("logins for the email = %+v, want one with hash-one, temporary", got)
	}
	if account, _ := linkOf(t1, p1); account != got[0].id {
		t.Errorf("person in business 1 linked to %q, want %s", account, got[0].id)
	}

	if n := run(t2); n != 1 {
		t.Errorf("a different password in business 2: %d left unlinked, want 1", n)
	}
	if account, hash := linkOf(t2, p2); account != "" || hash != "hash-two" {
		t.Errorf("person in business 2 = login %q, hash %q; want unlinked with hash-two kept", account, hash)
	}

	if n := run(t3); n != 0 {
		t.Errorf("the same password in business 3: %d left unlinked, want 0", n)
	}
	if account, _ := linkOf(t3, p3); account != got[0].id {
		t.Errorf("person in business 3 linked to %q, want %s", account, got[0].id)
	}

	t.Run("running it again changes nothing", func(t *testing.T) {
		if a, b, c := run(t1), run(t2), run(t3); a != 0 || b != 1 || c != 0 {
			t.Errorf("rerun = %d, %d, %d unlinked; want 0, 1, 0", a, b, c)
		}
		if again := logins(); !slices.Equal(again, got) {
			t.Errorf("logins after the rerun = %+v, want %+v", again, got)
		}
		if account, _ := linkOf(t2, p2); account != "" {
			t.Error("the rerun linked the person with a different password")
		}
	})

	t.Run("a disabled person gets no login, and doesn't take the email from an active one", func(t *testing.T) {
		gone := testdb.Email("gone")
		older, _ := testdb.NewTenant(t, owner, "Carry-over Disabled")
		newer, _ := testdb.NewTenant(t, owner, "Carry-over Active")
		p := addPerson(t, owner, older, "Gone Person", gone, "old-hash", false)
		if err := db.WithTenant(ctx, owner, older, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE persons SET status = 'disabled' WHERE id = $1", p)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		q := addPerson(t, owner, newer, "Active Person", gone, "new-hash", false)
		if n := run(older); n != 0 {
			t.Errorf("business with a disabled person: %d left unlinked, want 0", n)
		}
		if account, _ := linkOf(older, p); account != "" {
			t.Errorf("the disabled person was linked to %q", account)
		}
		var logins int
		if err := owner.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE email = $1", gone).Scan(&logins); err != nil || logins != 0 {
			t.Errorf("a disabled person's email has %d logins (%v), want 0", logins, err)
		}
		if n := run(newer); n != 0 {
			t.Errorf("the active person after a disabled one: %d left unlinked, want 0", n)
		}
		var hash string
		if err := owner.QueryRow(ctx, "SELECT password_hash FROM accounts WHERE email = $1", gone).Scan(&hash); err != nil || hash != "new-hash" {
			t.Errorf("the email's login has hash %q (%v), want the active person's new-hash", hash, err)
		}
		if account, _ := linkOf(newer, q); account == "" {
			t.Error("the active person was left unlinked")
		}
	})

	t.Run("it gives back the business that was open", func(t *testing.T) {
		err := db.WithTenant(ctx, owner, t2, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT link_existing_logins($1)", t1); err != nil {
				return err
			}
			var current string
			if err := tx.QueryRow(ctx, "SELECT current_tenant()::text").Scan(&current); err != nil {
				return err
			}
			if current != t2 {
				t.Errorf("after the carry-over, the open business is %q, want %s", current, t2)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// visiblePeople returns the ids of the people the app sees with scope s.
func visiblePeople(t *testing.T, app *pgxpool.Pool, s db.Scope) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	err := pgx.BeginFunc(ctx, app, func(tx pgx.Tx) error {
		if err := db.SetScope(ctx, tx, s); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, "SELECT id::text FROM persons")
		var err error
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// sameSet reports whether got holds exactly the ids want, in any order.
func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, id := range want {
		if !slices.Contains(got, id) {
			return false
		}
	}
	return true
}

// addPerson adds a person to a business and returns their id. A nil email or hash leaves it empty;
// a hash is an old per-business password, as before migration 00015.
func addPerson(t *testing.T, owner *pgxpool.Pool, tenant, name string, email, hash any, temporary bool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO persons (tenant_id, display_name, login_email, password_hash, password_temporary)
			VALUES ($1, $2, $3, $4, $5) RETURNING id::text`, tenant, name, email, hash, temporary).Scan(&id)
	})
	if err != nil {
		t.Fatalf("add person: %v", err)
	}
	return id
}
