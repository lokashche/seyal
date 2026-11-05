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
	return l.use(b, code, auth.LinkUse{Mode: auth.ModeChoose, Password: password, UserAgent: "test browser"})
}

func (l login) use(b biz, code string, u auth.LinkUse) (token string, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		token, _, err = auth.UseLink(context.Background(), tx, code, u)
		return err
	})
	return token, err
}

func (l login) linkHolder(b biz, code, token string) (h auth.Holder, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		h, err = auth.LinkHolder(context.Background(), tx, code, token)
		return err
	})
	return h, err
}

func (l login) linkMode(b biz, personID string) (m auth.Mode, err error) {
	err = l.in(b, func(tx pgx.Tx) error {
		var err error
		m, err = auth.LinkModeOf(context.Background(), tx, personID)
		return err
	})
	return m, err
}

// TestUseLink: a set-your-password link works on the person's login. It may make one, or set the
// password of the person's own login when no other business uses it, never of any other (D64).
func TestUseLink(t *testing.T) {
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

	t.Run("a login in no business: the link only joins it, with its password", func(t *testing.T) {
		l := newLogin(t) // made by the operator, in no business
		b := newBiz(t, owner, "Cobalt Couriers (link test)", l.email)
		code := newLinkFor(t, l, b)
		if h, err := l.linkHolder(b, code, ""); err != nil || h.Mode != auth.ModeJoin {
			t.Errorf("link holder = %+v, %v; want join: the login isn't this business's to reset", h, err)
		}
		if _, err := l.useLink(b, code, "cobalt password 1"); !errors.Is(err, auth.ErrLinkChanged) {
			t.Errorf("choosing a password = %v, want ErrLinkChanged", err)
		}
		if _, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeJoin, Password: l.password}); err != nil {
			t.Fatal(err)
		}
		l.logIn(t) // the operator's password, unchanged
		l.wantEvents(t, b, "person.account_linked", 1)
	})

	t.Run("forgot password, but the login is also used in another business: refused", func(t *testing.T) {
		l := newLogin(t, "Delta Dairy (link test)", "Echo Exports (link test)")
		y := l.biz[0]
		code := newLinkFor(t, l, y)
		if h, err := l.linkHolder(y, code, ""); err != nil || h.Mode != auth.ModeRefused {
			t.Errorf("link holder = %+v, %v; want refused", h, err)
		}
		if m, err := l.linkMode(y, y.person); err != nil || m != auth.ModeRefused {
			t.Errorf("the mode of a new link = %q, %v; want refused, never choose", m, err)
		}
		for _, mode := range []auth.Mode{auth.ModeChoose, auth.ModeJoin} {
			_, err := l.use(y, code, auth.LinkUse{Mode: mode, Password: "taken over password"})
			if !errors.Is(err, auth.ErrLinkedElsewhere) {
				t.Errorf("use link as %q = %v, want ErrLinkedElsewhere", mode, err)
			}
		}
		l.logIn(t) // the password is unchanged
		l.wantEvents(t, y, "person.link_used", 0)
	})
}
