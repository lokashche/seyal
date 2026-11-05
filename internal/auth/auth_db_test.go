//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/ledger"
	"seyal/internal/testdb"
)

// oldSession starts a session the way the old per-business login did and returns its token.
func oldSession(t *testing.T, l login, b biz, expires time.Time) string {
	t.Helper()
	token := rand.Text()
	sum := sha256.Sum256([]byte(token))
	sqlIn(t, l.owner, b.tenant, `INSERT INTO sessions (token_sha256, tenant_id, person_id, expires_at)
		VALUES ($1, current_tenant(), $2, $3)`, hex.EncodeToString(sum[:]), b.person, expires)
	return token
}

func (l login) upgrade(b biz, old string) (token string, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		token, _, err = auth.Upgrade(context.Background(), tx, old, "test browser")
		return err
	})
	return token, err
}

// TestUpgrade: for one release, an old per-business session of a person with a login becomes a
// login session, once (D64).
func TestUpgrade(t *testing.T) {
	l := newLogin(t, "Acme Traders (upgrade test)", "Beacon Books (upgrade test)")
	a, b := l.biz[0], l.biz[1]

	t.Run("a linked person's old session becomes a login session, once", func(t *testing.T) {
		old := oldSession(t, l, a, time.Now().Add(time.Hour))
		token, err := l.upgrade(a, old)
		if err != nil || token == "" {
			t.Fatalf("upgrade = %q, %v", token, err)
		}
		for _, x := range []biz{a, b} { // a login session, so it works in both
			if s, err := l.checkIn(x, token); err != nil || s.PersonID != x.person {
				t.Errorf("check in %s with the new token = %+v, %v", x.tenant, s, err)
			}
		}
		_, err = l.upgrade(a, old)
		wantNoSession(t, "the old session a second time", err)
	})

	t.Run("an old session belongs to its own business", func(t *testing.T) {
		_, err := l.upgrade(b, oldSession(t, l, a, time.Now().Add(time.Hour)))
		wantNoSession(t, "another business's old session", err)
	})

	t.Run("logging out ends the old sessions in every business too", func(t *testing.T) {
		oldA, oldB := oldSession(t, l, a, time.Now().Add(time.Hour)), oldSession(t, l, b, time.Now().Add(time.Hour))
		token, err := l.upgrade(a, oldA)
		if err != nil {
			t.Fatal(err)
		}
		before := len(l.events(t, b, "person.logged_out"))
		if err := l.server(func(tx pgx.Tx) error { return auth.LogOut(context.Background(), tx, token) }); err != nil {
			t.Fatal(err)
		}
		_, err = l.upgrade(b, oldB)
		wantNoSession(t, "another business's old session after logging out", err)
		l.wantEvents(t, b, "person.logged_out", before+1) // an old session ended there
	})

	t.Run("linking a person ends their sessions from before", func(t *testing.T) {
		u := newLogin(t, "Fennel Foods (upgrade test)")
		x := u.biz[0]
		sqlIn(t, u.owner, x.tenant, "UPDATE persons SET account_id = NULL WHERE id = $1", x.person)
		old := oldSession(t, u, x, time.Now().Add(time.Hour))
		if err := u.in(x, func(tx pgx.Tx) error {
			return auth.Link(context.Background(), tx, u.id, x.person, "operator", ledger.SystemActor)
		}); err != nil {
			t.Fatal(err)
		}
		_, err := u.upgrade(x, old)
		wantNoSession(t, "an old session from before the link", err)
	})

	t.Run("expired, unlinked or disabled: log in again", func(t *testing.T) {
		_, err := l.upgrade(a, oldSession(t, l, a, time.Now().Add(-time.Minute)))
		wantNoSession(t, "an expired old session", err)

		u := newLogin(t, "Cobalt Couriers (upgrade test)")
		old := oldSession(t, u, u.biz[0], time.Now().Add(time.Hour))
		sqlIn(t, u.owner, u.biz[0].tenant, "UPDATE persons SET account_id = NULL WHERE id = $1", u.biz[0].person)
		_, err = u.upgrade(u.biz[0], old)
		wantNoSession(t, "an unlinked person's old session", err)

		d := newLogin(t, "Delta Dairy (upgrade test)")
		old = oldSession(t, d, d.biz[0], time.Now().Add(time.Hour))
		sqlIn(t, d.owner, d.biz[0].tenant, "UPDATE persons SET status = 'disabled' WHERE id = $1", d.biz[0].person)
		_, err = d.upgrade(d.biz[0], old)
		wantNoSession(t, "a disabled person's old session", err)

		_, err = l.upgrade(a, "")
		wantNoSession(t, "no cookie", err)
	})
}

// newLinkFor makes a set-your-password link for the person of b, as b's admin would.
func newLinkFor(t *testing.T, l login, b biz) string {
	t.Helper()
	var code string
	if err := l.in(b, func(tx pgx.Tx) error {
		var err error
		code, _, err = auth.NewLink(context.Background(), tx, b.person, ledger.SystemActor)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return code
}

func (l login) useLink(b biz, code, password string) (token string, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		token, _, err = auth.UseLink(context.Background(), tx, code, password, "test browser")
		return err
	})
	return token, err
}

func (l login) linkHolder(b biz, code string) (h auth.Holder, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		h, err = auth.LinkHolder(context.Background(), tx, code)
		return err
	})
	return h, err
}

// TestUseLink: a set-your-password link works on the person's login. It may make one, or set the
// password of one that is in no other business, never of one that is (D64).
func TestUseLink(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)

	t.Run("a new email gets a login, linked to the person", func(t *testing.T) {
		l := login{owner: owner, app: app, email: testdb.Email("neha"), password: "neha own password"}
		b := newBiz(t, owner, "Acme Traders (link test)", l.email)
		token, err := l.useLink(b, newLinkFor(t, l, b), l.password)
		if err != nil {
			t.Fatal(err)
		}
		if s, err := l.checkIn(b, token); err != nil || s.PersonID != b.person || s.Email != l.email {
			t.Errorf("check in = %+v, %v", s, err)
		}
		l.logIn(t)
		l.wantEvents(t, b, "person.account_linked", 1)
		l.wantEvents(t, b, "person.login_set", 1)
		l.wantEvents(t, b, "person.link_used", 1)
		for _, body := range l.events(t, b, "person.login_set") {
			if strings.Contains(body, l.password) || strings.Contains(body, "$2a$") {
				t.Errorf("the ledger holds the password or its hash: %s", body)
			}
		}
	})

	t.Run("a login in this business only: its password is set", func(t *testing.T) {
		l := newLogin(t, "Beacon Books (link test)")
		b := l.biz[0]
		before := l.logIn(t)
		token, err := l.useLink(b, newLinkFor(t, l, b), "a brand new password")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.checkIn(b, token); err != nil {
			t.Errorf("check in with the new token = %v", err)
		}
		_, err = l.checkIn(b, before)
		wantNoSession(t, "a session from before the new password", err)
		if _, _, err := l.tryLogIn(l.email, l.password); !errors.Is(err, auth.ErrBadLogin) {
			t.Errorf("the old password = %v, want ErrBadLogin", err)
		}
		l.password = "a brand new password"
		l.logIn(t)
	})

	t.Run("a login in no business yet: its password is set and it is linked", func(t *testing.T) {
		l := newLogin(t) // made by the operator, in no business
		b := newBiz(t, owner, "Cobalt Couriers (link test)", l.email)
		if _, err := l.useLink(b, newLinkFor(t, l, b), "cobalt password 1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := l.tryLogIn(l.email, "cobalt password 1"); err != nil {
			t.Errorf("log in with the link's password = %v", err)
		}
	})

	t.Run("a login also used in another business is refused, before and when used", func(t *testing.T) {
		l := newLogin(t, "Delta Dairy (link test)")
		x := newBiz(t, owner, "Echo Exports (link test)", l.email) // not linked yet: a new person here
		code := newLinkFor(t, l, x)
		if h, err := l.linkHolder(x, code); err != nil || !h.Elsewhere || h.Linked {
			t.Errorf("link holder = %+v, %v; want elsewhere, not linked here", h, err)
		}
		if _, err := l.useLink(x, code, "taken over password"); !errors.Is(err, auth.ErrLinkedElsewhere) {
			t.Errorf("use link = %v, want ErrLinkedElsewhere", err)
		}
		l.logIn(t) // the password is unchanged
		l.wantEvents(t, x, "person.account_linked", 0)

		// The person was linked in both: a forgotten password goes to the server's operator.
		y := l.biz[0]
		if _, err := l.useLink(y, newLinkFor(t, l, y), "taken over password"); err != nil {
			t.Fatalf("the only business's link = %v", err)
		}
		l.password = "taken over password"
		if err := l.in(x, func(tx pgx.Tx) error {
			return auth.Link(ctx, tx, l.id, x.person, "operator", ledger.SystemActor)
		}); err != nil {
			t.Fatal(err)
		}
		code = newLinkFor(t, l, y)
		if h, err := l.linkHolder(y, code); err != nil || !h.Elsewhere || !h.Linked {
			t.Errorf("link holder = %+v, %v; want elsewhere and linked here", h, err)
		}
		if _, err := l.useLink(y, code, "yet another password"); !errors.Is(err, auth.ErrLinkedElsewhere) {
			t.Errorf("a linked person's link, the login now in two = %v, want ErrLinkedElsewhere", err)
		}
		l.logIn(t)
	})
}
