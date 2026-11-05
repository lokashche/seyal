//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/audit"
	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/testdb"
)

// login is a test login linked to a person in each of its businesses.
type login struct {
	owner, app *pgxpool.Pool
	id         string
	email      string
	password   string
	biz        []biz
}

// biz is a test business and the login's person there.
type biz struct{ tenant, person string }

// newLogin makes a login with `seyal set-password` semantics and links it, as the operator would, to
// a person in a new business for each name.
func newLogin(t *testing.T, names ...string) login {
	t.Helper()
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	l := login{owner: owner, app: app, email: testdb.Email("asha")}
	err := db.WithServer(ctx, app, func(tx pgx.Tx) error {
		var err error
		l.password, l.id, err = auth.NewPassword(ctx, tx, strings.ToUpper(l.email), "")
		return err
	})
	if err != nil {
		t.Fatalf("new login: %v", err)
	}
	for _, name := range names {
		b := newBiz(t, owner, name, l.email)
		if err := l.in(b, func(tx pgx.Tx) error {
			return auth.Link(ctx, tx, l.id, b.person, "operator", ledger.SystemActor)
		}); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		l.biz = append(l.biz, b)
	}
	return l
}

// newBiz makes a business whose one person has this email, not linked to any login.
func newBiz(t *testing.T, owner *pgxpool.Pool, name, email string) biz {
	t.Helper()
	tenant, person := testdb.NewTenant(t, owner, name)
	sqlIn(t, owner, tenant, "UPDATE persons SET login_email = $1 WHERE id = $2", email, person)
	return biz{tenant, person}
}

func (l login) in(b biz, fn func(pgx.Tx) error) error {
	return db.WithTenant(context.Background(), l.app, b.tenant, fn)
}

func (l login) server(fn func(pgx.Tx) error) error {
	return db.WithServer(context.Background(), l.app, fn)
}

func (l login) logIn(t *testing.T) string {
	t.Helper()
	token, _, err := l.tryLogIn(l.email, l.password)
	if err != nil {
		t.Fatalf("log in: %v", err)
	}
	return token
}

func (l login) tryLogIn(email, password string) (token string, a auth.Account, err error) {
	err = l.server(func(tx pgx.Tx) error {
		var err error
		token, a, err = auth.LogIn(context.Background(), tx, email, password, "test browser")
		return err
	})
	return token, a, err
}

func (l login) checkIn(b biz, token string) (s auth.Session, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		s, err = auth.Check(context.Background(), tx, token, "test browser")
		return err
	})
	return s, err
}

func (l login) checkAccount(token string) (a auth.Account, err error) {
	err = l.server(func(tx pgx.Tx) error {
		var err error
		a, err = auth.CheckAccount(context.Background(), tx, token)
		return err
	})
	return a, err
}

// events returns the bodies of a business's ledger events of one kind, oldest first.
func (l login) events(t *testing.T, b biz, kind string) []string {
	t.Helper()
	var bodies []string
	if err := l.in(b, func(tx pgx.Tx) error {
		rows, _ := tx.Query(context.Background(), "SELECT body FROM ledger WHERE kind = $1 ORDER BY seq", kind)
		var err error
		bodies, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return bodies
}

func (l login) wantEvents(t *testing.T, b biz, kind string, n int) {
	t.Helper()
	if got := l.events(t, b, kind); len(got) != n {
		t.Errorf("%s events in %s = %d, want %d: %v", kind, b.tenant, len(got), n, got)
	}
}

// sqlIn runs one statement as the owner inside a business.
func sqlIn(t *testing.T, owner *pgxpool.Pool, tenant, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// first returns the business whose id sorts first. A change fanned out to every business from inside
// it visits the other one last, so a scope not put back afterwards would show.
func first(a, b biz) biz {
	if b.tenant < a.tenant {
		return b
	}
	return a
}

// wantScope fails unless tx still has scope want, e.g. after a change fanned out to every business.
func wantScope(tx pgx.Tx, want db.Scope) error {
	got, err := db.CurrentScope(context.Background(), tx)
	if err == nil && got != want {
		err = fmt.Errorf("scope after = %+v, want %+v", got, want)
	}
	return err
}

func wantNoSession(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("%s = %v, want ErrNoSession", what, err)
	}
}

// TestOneLoginManyBusinesses: one login reaches its person in each business it belongs to, and
// nothing in any other business (D64).
func TestOneLoginManyBusinesses(t *testing.T) {
	l := newLogin(t, "Acme Traders (login test)", "Beacon Books (login test)")
	a, b := l.biz[0], l.biz[1]
	c := newBiz(t, l.owner, "Cobalt Couriers (login test)", testdb.Email("someone"))
	token := l.logIn(t)

	for _, x := range []biz{a, b} {
		s, err := l.checkIn(x, token)
		if err != nil || s.PersonID != x.person || s.AccountID != l.id || s.Email != l.email || s.MustChange {
			t.Errorf("check in %s = %+v, %v; want person %s of login %s", x.tenant, s, err, x.person, l.id)
		}
	}
	_, err := l.checkIn(c, token)
	wantNoSession(t, "check in a business the login isn't in", err)

	t.Run("pages outside a business see the login", func(t *testing.T) {
		got, err := l.checkAccount(token)
		if err != nil || got.ID != l.id || got.Email != l.email || got.ExpiresAt.Before(time.Now()) {
			t.Errorf("check account = %+v, %v", got, err)
		}
		_, err = l.checkAccount("")
		wantNoSession(t, "no token", err)
	})

	t.Run("an old per-business cookie can't be made from a login token or entry", func(t *testing.T) {
		// Anyone who can read the database knows a login session's hash and the business's id; an
		// old-style cookie made from them must open nothing (see legacy.go).
		sum := sha256.Sum256([]byte(token))
		for _, cookie := range []string{token, hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:]) + ":" + a.tenant} {
			err := l.in(a, func(tx pgx.Tx) error {
				_, _, err := auth.Upgrade(context.Background(), tx, cookie, "test browser")
				return err
			})
			wantNoSession(t, "upgrade of "+cookie, err)
		}
	})

	t.Run("businesses are listed by name, and the scope is put back", func(t *testing.T) {
		var got []auth.Business
		var tenantAfter string
		err := l.in(c, func(tx pgx.Tx) error { // even from inside another business
			var err error
			if got, err = auth.Businesses(context.Background(), tx, l.id); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), "SELECT current_tenant()::text").Scan(&tenantAfter)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].TenantID != a.tenant || got[0].PersonID != a.person || !got[0].Active ||
			got[1].TenantID != b.tenant || got[1].Name != "Beacon Books (login test)" || got[1].Slug == "" {
			t.Errorf("businesses = %+v", got)
		}
		if tenantAfter != c.tenant {
			t.Errorf("scope after = %q, want business %s again", tenantAfter, c.tenant)
		}
	})

	t.Run("linked elsewhere", func(t *testing.T) {
		alone := newLogin(t, "Dahlia Designs (login test)")
		check := func(l login, in biz, want bool) {
			t.Helper()
			var got bool
			err := l.in(in, func(tx pgx.Tx) error {
				var err error
				got, err = auth.LinkedElsewhere(context.Background(), tx, l.id)
				return err
			})
			if err != nil || got != want {
				t.Errorf("linked elsewhere than %s = %v, %v; want %v", in.tenant, got, err, want)
			}
		}
		check(l, a, true)
		check(l, c, true)
		check(alone, alone.biz[0], false)
		check(alone, c, true) // in exactly one other business
		// A disabled person counts too: they may be enabled again.
		sqlIn(t, alone.owner, alone.biz[0].tenant, "UPDATE persons SET status = 'disabled' WHERE id = $1", alone.biz[0].person)
		check(alone, c, true)
	})

	t.Run("disabled in one business is refused there only", func(t *testing.T) {
		if err := l.in(a, func(tx pgx.Tx) error {
			_, err := people.SetActive(context.Background(), tx, a.person, false, ledger.SystemActor)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, err := l.checkIn(a, token)
		wantNoSession(t, "check in where disabled", err)
		if _, err := l.checkIn(b, token); err != nil {
			t.Errorf("check in the other business = %v", err)
		}
		var bs []auth.Business
		if err := l.server(func(tx pgx.Tx) error {
			var err error
			bs, err = auth.Businesses(context.Background(), tx, l.id)
			return err
		}); err != nil || len(bs) != 2 || bs[0].Active || !bs[1].Active {
			t.Errorf("businesses while disabled in one = %+v, %v", bs, err)
		}
		if err := l.in(a, func(tx pgx.Tx) error {
			_, err := people.SetActive(context.Background(), tx, a.person, true, ledger.SystemActor)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.checkIn(a, token); err != nil {
			t.Errorf("check in after enabling again = %v", err)
		}
	})

	t.Run("unlinked is refused at once", func(t *testing.T) {
		u := newLogin(t, "Ember Electric (login test)", "Fern Farms (login test)")
		tok := u.logIn(t)
		if _, err := u.checkIn(u.biz[0], tok); err != nil {
			t.Fatal(err)
		}
		sqlIn(t, u.owner, u.biz[0].tenant, "UPDATE persons SET account_id = NULL WHERE id = $1", u.biz[0].person)
		_, err := u.checkIn(u.biz[0], tok)
		wantNoSession(t, "check in after unlinking", err)
		if _, err := u.checkIn(u.biz[1], tok); err != nil {
			t.Errorf("check in the other business = %v", err)
		}
	})

	t.Run("an ended or expired login session doesn't work", func(t *testing.T) {
		for _, set := range []string{"ended_at = now()", "expires_at = now() - interval '1 minute'"} {
			tok := l.logIn(t)
			if _, err := l.owner.Exec(context.Background(), "UPDATE account_sessions SET "+set+
				" WHERE token_sha256 = encode(sha256(convert_to($1, 'UTF8')), 'hex')", tok); err != nil {
				t.Fatal(err)
			}
			_, err := l.checkIn(b, tok)
			wantNoSession(t, set+": check in", err)
			_, err = l.checkAccount(tok)
			wantNoSession(t, set+": check account", err)
		}
	})
}

func TestLogIn(t *testing.T) {
	l := newLogin(t, "Garnet Goods (login test)")

	t.Run("wrong password and unknown email look the same", func(t *testing.T) {
		for _, tc := range [][2]string{
			{l.email, "wrong password!!"},
			{l.email, ""},
			{testdb.Email("nobody"), l.password},
			{"", l.password},
		} {
			if _, _, err := l.tryLogIn(tc[0], tc[1]); !errors.Is(err, auth.ErrBadLogin) {
				t.Errorf("log in %q / %q = %v, want ErrBadLogin", tc[0], tc[1], err)
			}
		}
	})

	t.Run("the email may have capitals and spaces", func(t *testing.T) {
		token, a, err := l.tryLogIn("  "+strings.ToUpper(l.email)+" ", l.password)
		if err != nil || token == "" || a.ID != l.id || a.Email != l.email || a.MustChange {
			t.Errorf("log in = %q, %+v, %v", token, a, err)
		}
	})

	t.Run("logging in writes nothing to any ledger", func(t *testing.T) {
		l.logIn(t)
		l.wantEvents(t, l.biz[0], "person.logged_in", 0)
	})

	t.Run("a login session's token itself is not stored", func(t *testing.T) {
		token := l.logIn(t)
		var n int
		if err := l.server(func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), "SELECT count(*) FROM account_sessions WHERE token_sha256 = $1", token).Scan(&n)
		}); err != nil || n != 0 {
			t.Errorf("found the raw token in account_sessions (%d rows, %v)", n, err)
		}
	})

	t.Run("must-change comes from the login", func(t *testing.T) {
		token := l.logIn(t)
		if _, err := l.owner.Exec(context.Background(), "UPDATE accounts SET password_temporary = true WHERE id = $1", l.id); err != nil {
			t.Fatal(err)
		}
		if s, err := l.checkIn(l.biz[0], token); err != nil || !s.MustChange {
			t.Errorf("check in = %+v, %v; want MustChange", s, err)
		}
		if a, err := l.checkAccount(token); err != nil || !a.MustChange {
			t.Errorf("check account = %+v, %v; want MustChange", a, err)
		}
		if _, a, err := l.tryLogIn(l.email, l.password); err != nil || !a.MustChange {
			t.Errorf("log in = %+v, %v; want MustChange", a, err)
		}
	})

	t.Run("a new password from Seyal is never temporary", func(t *testing.T) {
		if _, err := l.owner.Exec(context.Background(), "UPDATE accounts SET password_temporary = true WHERE id = $1", l.id); err != nil {
			t.Fatal(err)
		}
		var pw, id string
		if err := l.server(func(tx pgx.Tx) error {
			var err error
			pw, id, err = auth.NewPassword(context.Background(), tx, l.email, "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if id != l.id || len(pw) != 16 || pw == l.password {
			t.Errorf("new password = %q for %s; want a new 16-character password for %s", pw, id, l.id)
		}
		if _, _, err := l.tryLogIn(l.email, l.password); !errors.Is(err, auth.ErrBadLogin) {
			t.Errorf("old password = %v, want ErrBadLogin", err)
		}
		_, a, err := l.tryLogIn(l.email, pw)
		if err != nil || a.MustChange {
			t.Errorf("log in with the new password = %+v, %v; want a permanent password", a, err)
		}
		var temporary bool
		if err := l.owner.QueryRow(context.Background(), "SELECT password_temporary FROM accounts WHERE id = $1", l.id).
			Scan(&temporary); err != nil || temporary {
			t.Errorf("password_temporary = %v, %v; want false", temporary, err)
		}
		// Seyal set it, in the login's one business.
		got := l.events(t, l.biz[0], "person.login_set")
		if len(got) != 1 || !strings.Contains(got[0], `"actor_kind":"system"`) || !strings.Contains(got[0], `"email":"`+l.email+`"`) {
			t.Errorf("login_set events = %v; want one by Seyal with the email", got)
		}
	})

	t.Run("a new login is made for a new email", func(t *testing.T) {
		email := testdb.Email("new")
		var pw, id string
		if err := l.server(func(tx pgx.Tx) error {
			var err error
			pw, id, err = auth.NewPassword(context.Background(), tx, email, "")
			return err
		}); err != nil || id == "" {
			t.Fatalf("new login = %q, %v", id, err)
		}
		if _, a, err := l.tryLogIn(email, pw); err != nil || a.ID != id || a.MustChange {
			t.Errorf("log in = %+v, %v", a, err)
		}
		if err := l.server(func(tx pgx.Tx) error {
			_, _, err := auth.NewPassword(context.Background(), tx, "not an email", "")
			return err
		}); err == nil {
			t.Error("a login without an @ was made")
		}
	})
}

// TestFirstRequestInABusiness: a login session's first request in a business records one entry and
// one person.logged_in, even when two requests arrive at once.
func TestFirstRequestInABusiness(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Harbour Hardware (login test)")
	a := l.biz[0]

	t.Run("once per business per session", func(t *testing.T) {
		token := l.logIn(t)
		for range 3 {
			if _, err := l.checkIn(a, token); err != nil {
				t.Fatal(err)
			}
		}
		l.wantEvents(t, a, "person.logged_in", 1)
		if _, err := l.checkIn(a, l.logIn(t)); err != nil { // another device: its own entry
			t.Fatal(err)
		}
		l.wantEvents(t, a, "person.logged_in", 2)
	})

	t.Run("two requests at once", func(t *testing.T) {
		before := len(l.events(t, a, "person.logged_in"))
		token := l.logIn(t)

		// The first request enters the business and holds its transaction open...
		first, err := l.app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback(ctx)
		if err := db.SetScope(ctx, first, db.Scope{Tenant: a.tenant}); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.Check(ctx, first, token, "first"); err != nil {
			t.Fatal(err)
		}
		// ...while the second, not seeing the first's entry yet, tries to add its own.
		second := make(chan error, 1)
		go func() {
			_, err := l.checkIn(a, token)
			second <- err
		}()
		waitForLock(t, l.app, "%INSERT INTO sessions%account_session%")
		if err := first.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Errorf("second request = %v", err)
		}
		l.wantEvents(t, a, "person.logged_in", before+1)
		var entries int
		if err := l.in(a, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE account_session = encode(sha256(convert_to($1, 'UTF8')), 'hex')", token).Scan(&entries)
		}); err != nil || entries != 1 {
			t.Errorf("entries = %d, %v; want 1", entries, err)
		}
	})

	t.Run("an ended entry is entered again", func(t *testing.T) {
		in, out := len(l.events(t, a, "person.logged_in")), len(l.events(t, a, "person.logged_out"))
		token := l.logIn(t)
		if _, err := l.checkIn(a, token); err != nil {
			t.Fatal(err)
		}
		// Unlinked, with the person's sessions ended (as an email change will do), then linked again.
		sqlIn(t, l.owner, a.tenant, "UPDATE persons SET account_id = NULL WHERE id = $1", a.person)
		sqlIn(t, l.owner, a.tenant, "UPDATE sessions SET ended_at = now() WHERE person_id = $1 AND ended_at IS NULL", a.person)
		if err := l.in(a, func(tx pgx.Tx) error {
			return auth.Link(ctx, tx, l.id, a.person, "link", ledger.SystemActor)
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := l.checkIn(a, token); err != nil {
				t.Fatal(err)
			}
		}
		l.wantEvents(t, a, "person.logged_in", in+2)
		if err := l.server(func(tx pgx.Tx) error { return auth.LogOut(ctx, tx, token) }); err != nil {
			t.Fatal(err)
		}
		l.wantEvents(t, a, "person.logged_out", out+1)
	})
}

// waitForLock waits until some transaction of the app is waiting on a lock while running a
// statement like one of these patterns, i.e. the second request is really waiting for the first.
func waitForLock(t *testing.T, app *pgxpool.Pool, patterns ...string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if err := app.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE usename = current_user AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
				AND query LIKE ANY ($1)`, patterns).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
	}
	t.Fatal("the second request never waited for the first")
}

// TestLogInDuringAPasswordChange: logging in with the old password while a change is being written
// waits for the change, and then fails, so no session outlives the change.
func TestLogInDuringAPasswordChange(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Quartz Quarry (login test)")
	change, err := l.app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer change.Rollback(ctx)
	if err := auth.ChangePassword(ctx, change, l.id, l.password, "quartz password 2"); err != nil {
		t.Fatal(err)
	}
	login := make(chan error, 1)
	go func() {
		_, _, err := l.tryLogIn(l.email, l.password)
		login <- err
	}()
	// Without the wait, the login would check the old hash at once and block only on adding its session.
	waitForLock(t, l.app, "%FROM accounts%FOR SHARE%", "%INSERT INTO account_sessions%")
	if err := change.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-login; !errors.Is(err, auth.ErrBadLogin) {
		t.Errorf("log in with the old password during the change = %v, want ErrBadLogin", err)
	}
	var open int
	if err := l.owner.QueryRow(ctx, "SELECT count(*) FROM account_sessions WHERE account_id = $1 AND ended_at IS NULL", l.id).
		Scan(&open); err != nil || open != 0 {
		t.Errorf("open login sessions = %d, %v; want 0", open, err)
	}
}

func TestLogOut(t *testing.T) {
	l := newLogin(t, "Indigo Ink (login test)", "Juniper Joinery (login test)")
	a, b := l.biz[0], l.biz[1]

	t.Run("logging out ends the login everywhere, recorded where it was used", func(t *testing.T) {
		token, other := l.logIn(t), l.logIn(t)
		if _, err := l.checkIn(a, token); err != nil {
			t.Fatal(err)
		}
		if _, err := l.checkIn(b, other); err != nil {
			t.Fatal(err)
		}
		if err := l.server(func(tx pgx.Tx) error { return auth.LogOut(context.Background(), tx, token) }); err != nil {
			t.Fatal(err)
		}
		for _, x := range []biz{a, b} {
			_, err := l.checkIn(x, token)
			wantNoSession(t, "check in after logging out", err)
		}
		_, err := l.checkAccount(token)
		wantNoSession(t, "check account after logging out", err)
		if _, err := l.checkIn(b, other); err != nil {
			t.Errorf("the other device = %v; want still logged in", err)
		}
		l.wantEvents(t, a, "person.logged_out", 1)
		l.wantEvents(t, b, "person.logged_out", 0) // this session never opened b
		if err := l.server(func(tx pgx.Tx) error { return auth.LogOut(context.Background(), tx, token) }); err != nil {
			t.Errorf("logging out again = %v", err)
		}
		l.wantEvents(t, a, "person.logged_out", 1)
		if err := l.server(func(tx pgx.Tx) error { return auth.LogOut(context.Background(), tx, other) }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("logging out everywhere ends every session in every business", func(t *testing.T) {
		one, two := l.logIn(t), l.logIn(t)
		if _, err := l.checkIn(a, one); err != nil {
			t.Fatal(err)
		}
		if _, err := l.checkIn(b, two); err != nil {
			t.Fatal(err)
		}
		from := first(a, b)
		if err := l.in(from, func(tx pgx.Tx) error { // from inside a business, as the Me page will
			if err := auth.LogOutEverywhere(context.Background(), tx, l.id); err != nil {
				return err
			}
			return wantScope(tx, db.Scope{Tenant: from.tenant})
		}); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{one, two} {
			for _, x := range []biz{a, b} {
				_, err := l.checkIn(x, tok)
				wantNoSession(t, "check in after logging out everywhere", err)
			}
		}
		// Each business had one open entry: a from one, b from two.
		for _, x := range []biz{a, b} {
			got := l.events(t, x, "person.logged_out_everywhere")
			if len(got) != 1 || !strings.Contains(got[0], `"sessions":1`) {
				t.Errorf("logged_out_everywhere in %s = %v; want one ending 1 session", x.tenant, got)
			}
		}
	})
}

func TestChangePassword(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Kestrel Kitchens (login test)", "Lotus Labs (login test)")
	a, b := l.biz[0], l.biz[1]
	phone, laptop := l.logIn(t), l.logIn(t)
	for _, x := range []biz{a, b} {
		if _, err := l.checkIn(x, phone); err != nil {
			t.Fatal(err)
		}
	}
	from := first(a, b)
	change := func(current, next string) error {
		return l.in(from, func(tx pgx.Tx) error {
			if err := auth.ChangePassword(ctx, tx, l.id, current, next); err != nil {
				return err
			}
			return wantScope(tx, db.Scope{Tenant: from.tenant})
		})
	}

	if err := change("wrong password!!", "a brand new password"); !errors.Is(err, auth.ErrWrongPassword) {
		t.Errorf("wrong current password = %v, want ErrWrongPassword", err)
	}
	if err := change(l.password, l.password); !errors.Is(err, auth.ErrSamePassword) {
		t.Errorf("same password = %v, want ErrSamePassword", err)
	}
	if err := change(l.password, "short"); err == nil {
		t.Error("a short new password was accepted")
	}
	const next = "a brand new password"
	if err := change(l.password, next); err != nil {
		t.Fatal(err)
	}

	for _, tok := range []string{phone, laptop} {
		_, err := l.checkAccount(tok)
		wantNoSession(t, "a session after the change", err)
		for _, x := range []biz{a, b} {
			_, err := l.checkIn(x, tok)
			wantNoSession(t, "a business session after the change", err)
		}
	}
	if _, _, err := l.tryLogIn(l.email, l.password); !errors.Is(err, auth.ErrBadLogin) {
		t.Errorf("old password = %v, want ErrBadLogin", err)
	}
	if _, _, err := l.tryLogIn(l.email, next); err != nil {
		t.Errorf("new password = %v", err)
	}
	for _, x := range []biz{a, b} {
		got := l.events(t, x, "person.login_set")
		if len(got) != 1 || !strings.Contains(got[0], `"actor":"`+x.person+`"`) || !strings.Contains(got[0], `"email":"`+l.email+`"`) {
			t.Errorf("login_set in %s = %v; want one by that business's person", x.tenant, got)
		}
		var open int
		if err := l.in(x, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE ended_at IS NULL").Scan(&open)
		}); err != nil || open != 0 {
			t.Errorf("open sessions in %s = %d, %v; want 0", x.tenant, open, err)
		}
	}
}

func TestLink(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Maple Mills (login test)")
	a := l.biz[0]
	link := func(x biz, accountID, personID string) error {
		return l.in(x, func(tx pgx.Tx) error {
			return auth.Link(ctx, tx, accountID, personID, "operator", ledger.SystemActor)
		})
	}
	addPerson := func(x biz, name, email string) string {
		var id string
		if err := l.in(x, func(tx pgx.Tx) error {
			var err error
			id, err = people.Add(ctx, tx, name, email, ledger.SystemActor)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("linking twice changes nothing", func(t *testing.T) {
		if err := link(a, l.id, a.person); err != nil {
			t.Fatal(err)
		}
		l.wantEvents(t, a, "person.account_linked", 1)
	})

	// Each refusal is checked by its message, so a case can't pass on some other rule.
	wantRefused := func(err error, because string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), because) {
			t.Errorf("link = %v; want refused because %q", err, because)
		}
	}

	t.Run("the email must be the login's", func(t *testing.T) {
		x := newBiz(t, l.owner, "Opal Orchard (login test)", testdb.Email("other")) // l isn't in it yet
		wantRefused(link(x, l.id, x.person), "isn't the login's email")
	})

	t.Run("a person's login is never moved", func(t *testing.T) {
		// m has the person's email and nobody in a yet, so only this rule stands in the way.
		m := newLogin(t)
		sqlIn(t, l.owner, a.tenant, "UPDATE persons SET login_email = $1 WHERE id = $2", m.email, a.person)
		defer sqlIn(t, l.owner, a.tenant, "UPDATE persons SET login_email = $1 WHERE id = $2", l.email, a.person)
		wantRefused(link(a, m.id, a.person), "already has another login")
	})

	t.Run("a disabled person isn't linked", func(t *testing.T) {
		x := newBiz(t, l.owner, "Nectar Nursery (login test)", l.email)
		sqlIn(t, l.owner, x.tenant, "UPDATE persons SET status = 'disabled' WHERE id = $1", x.person)
		wantRefused(link(x, l.id, x.person), "disabled")
	})

	t.Run("one person per login in a business", func(t *testing.T) {
		// The old email edit doesn't unlink yet (build part 4), so a second person can get the email.
		if err := l.in(a, func(tx pgx.Tx) error {
			_, err := people.Update(ctx, tx, a.person, "Renamed", testdb.Email("moved"), ledger.SystemActor)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		second := addPerson(a, "Second Person", l.email)
		wantRefused(link(a, l.id, second), "already reaches someone else")
	})

	t.Run("an unknown login", func(t *testing.T) {
		wantRefused(link(a, "00000000-0000-0000-0000-000000000000", a.person), "no such login")
	})
}

// TestTwoChainsStayApart runs a whole login's life across two businesses and checks both ledgers:
// their chains and tables are intact, no hash or password is in them, and neither names the other.
func TestTwoChainsStayApart(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Onyx Optics (login test)", "Pebble Press (login test)")
	a, b := l.biz[0], l.biz[1]

	phone, laptop := l.logIn(t), l.logIn(t)
	for _, step := range []func() error{
		func() error { _, err := l.checkIn(a, phone); return err },
		func() error { _, err := l.checkIn(b, phone); return err },
		func() error { _, err := l.checkIn(a, laptop); return err },
		func() error { return l.server(func(tx pgx.Tx) error { return auth.LogOut(ctx, tx, laptop) }) },
		func() error {
			return l.in(b, func(tx pgx.Tx) error {
				return auth.ChangePassword(ctx, tx, l.id, l.password, "pebble password 2")
			})
		},
		func() error {
			tok, _, err := l.tryLogIn(l.email, "pebble password 2")
			if err == nil {
				_, err = l.checkIn(b, tok)
			}
			return err
		},
		func() error {
			return l.server(func(tx pgx.Tx) error {
				if err := auth.LogOutEverywhere(ctx, tx, l.id); err != nil {
					return err
				}
				return wantScope(tx, db.Scope{}) // no business, as before
			})
		},
		func() error {
			return l.server(func(tx pgx.Tx) error {
				if _, _, err := auth.NewPassword(ctx, tx, l.email, ""); err != nil {
					return err
				}
				return wantScope(tx, db.Scope{})
			})
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ x, other biz }{{a, b}, {b, a}} {
		var rep ledger.Report
		var problems []audit.Problem
		var bodies []string
		if err := l.in(tc.x, func(tx pgx.Tx) error {
			var err error
			if rep, err = ledger.Verify(ctx, tx, ledger.Checkpoint{}); err != nil {
				return err
			}
			if problems, err = audit.CheckTables(ctx, tx); err != nil {
				return err
			}
			rows, _ := tx.Query(ctx, "SELECT body FROM ledger ORDER BY seq")
			bodies, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !rep.OK() || len(problems) > 0 {
			t.Errorf("business %s: verify = %+v, tables = %v", tc.x.tenant, rep.Problems, problems)
		}
		kinds := map[string]int{}
		for _, body := range bodies {
			for _, secret := range []string{"$2a$", l.password, "pebble password 2"} {
				if strings.Contains(body, secret) {
					t.Errorf("business %s: event holds a password or hash: %s", tc.x.tenant, body)
				}
			}
			for _, id := range []string{tc.other.tenant, tc.other.person, l.id} {
				if strings.Contains(body, id) {
					t.Errorf("business %s: event names %s from outside it: %s", tc.x.tenant, id, body)
				}
			}
			var e struct{ Kind string }
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				t.Fatal(err)
			}
			kinds[e.Kind]++
		}
		t.Logf("business %s: %d events %v", tc.x.tenant, len(bodies), kinds)
	}

	// a: in by phone and laptop, laptop out; b: in by phone and by the new login. Two password sets
	// (the change and Seyal's) and one log-out-everywhere in each.
	for _, tc := range []struct {
		x                           biz
		in, out, set, everywhere, n int
	}{{a, 2, 1, 2, 1, 1}, {b, 2, 0, 2, 1, 1}} {
		l.wantEvents(t, tc.x, "person.logged_in", tc.in)
		l.wantEvents(t, tc.x, "person.logged_out", tc.out)
		l.wantEvents(t, tc.x, "person.login_set", tc.set)
		l.wantEvents(t, tc.x, "person.logged_out_everywhere", tc.everywhere)
		l.wantEvents(t, tc.x, "person.account_linked", tc.n)
	}
}
