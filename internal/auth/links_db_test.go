//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/testdb"
)

// TestJoinLink: a link for someone whose email's login is used in another business only adds this
// business to the login, after its password (or its session in this browser). It never changes
// the password or ends a session (D64: no business can take over another's login).
func TestJoinLink(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Fennel Foods (join test)") // the victim's login, in a
	a := l.biz[0]
	inA := l.logIn(t)
	if _, err := l.checkIn(a, inA); err != nil {
		t.Fatal(err)
	}
	b := newBiz(t, l.owner, "Garnet Gardens (join test)", l.email) // b's admin added the same email
	code := newLinkFor(t, l, b)
	setInA := len(l.events(t, a, "person.login_set"))

	t.Run("the page asks for the login's password", func(t *testing.T) {
		h, err := l.linkHolder(b, code, "")
		if err != nil || h.Mode != auth.ModeJoin || h.Same || h.Other || h.Email != l.email {
			t.Errorf("link holder = %+v, %v; want join, not logged in", h, err)
		}
		if m, err := l.linkMode(b, b.person); err != nil || m != auth.ModeJoin {
			t.Errorf("mode of a new link = %q, %v", m, err)
		}
	})

	t.Run("a wrong password changes nothing", func(t *testing.T) {
		_, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeJoin, Password: "not the password"})
		if !errors.Is(err, auth.ErrWrongPassword) {
			t.Errorf("join with a wrong password = %v, want ErrWrongPassword", err)
		}
		l.wantEvents(t, b, "person.account_linked", 0)
		l.wantEvents(t, b, "person.link_used", 0)
		if _, err := l.linkHolder(b, code, ""); err != nil {
			t.Errorf("the link after a wrong password = %v; want it still usable", err)
		}
	})

	t.Run("choosing a password instead is refused: the page was out of date", func(t *testing.T) {
		_, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeChoose, Password: "taken over password"})
		if !errors.Is(err, auth.ErrLinkChanged) {
			t.Errorf("use a join link as choose = %v, want ErrLinkChanged", err)
		}
		l.logIn(t)
	})

	t.Run("another login in this browser is refused", func(t *testing.T) {
		other := newLogin(t)
		token := other.logIn(t)
		_, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeJoin, Password: l.password, Token: token})
		if !errors.Is(err, auth.ErrOtherLogin) {
			t.Errorf("join while logged in as someone else = %v, want ErrOtherLogin", err)
		}
		if h, err := l.linkHolder(b, code, token); err != nil || !h.Other || h.Same {
			t.Errorf("link holder for another login = %+v, %v", h, err)
		}
	})

	t.Run("the right password joins; the password and the other sessions stay", func(t *testing.T) {
		token, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeJoin, Password: l.password})
		if err != nil {
			t.Fatal(err)
		}
		if s, err := l.checkIn(b, token); err != nil || s.PersonID != b.person {
			t.Errorf("check in b = %+v, %v", s, err)
		}
		if _, err := l.checkIn(a, inA); err != nil {
			t.Errorf("the session in a from before = %v; a join ends no session", err)
		}
		l.logIn(t) // the same password
		l.wantEvents(t, b, "person.account_linked", 1)
		l.wantEvents(t, b, "person.link_used", 1)
		l.wantEvents(t, b, "person.login_set", 0)
		l.wantEvents(t, a, "person.login_set", setInA)
		for _, body := range l.events(t, b, "person.account_linked") {
			if !strings.Contains(body, `"how":"link"`) {
				t.Errorf("account_linked = %s, want how link", body)
			}
		}
		if _, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeJoin, Password: l.password}); !errors.Is(err, auth.ErrBadLink) {
			t.Errorf("the link a second time = %v, want ErrBadLink", err)
		}
	})

	t.Run("one tap when already logged in with the login", func(t *testing.T) {
		c := newBiz(t, l.owner, "Hazel Hats (join test)", l.email)
		code := newLinkFor(t, l, c)
		if h, err := l.linkHolder(c, code, inA); err != nil || !h.Same || h.Other || h.Mode != auth.ModeJoin {
			t.Errorf("link holder logged in = %+v, %v; want join, the same login", h, err)
		}
		token, err := l.use(c, code, auth.LinkUse{Mode: auth.ModeJoin, Token: inA})
		if err != nil || token != inA {
			t.Fatalf("one tap = %q, %v; want the browser's own session back", token, err)
		}
		if s, err := l.checkIn(c, inA); err != nil || s.PersonID != c.person {
			t.Errorf("check in the new business = %+v, %v", s, err)
		}
		var bs []auth.Business
		if err := l.server(func(tx pgx.Tx) error {
			var err error
			bs, err = auth.Businesses(ctx, tx, l.id)
			return err
		}); err != nil || len(bs) != 3 {
			t.Errorf("businesses = %d, %v; want 3", len(bs), err)
		}
	})

	checkLedger(t, l, a)
	checkLedger(t, l, b)
}

// checkLedger fails unless a business's chain is intact and its tables match the ledger.
func checkLedger(t *testing.T, l login, b biz) {
	t.Helper()
	if err := l.in(b, func(tx pgx.Tx) error {
		rep, err := ledger.Verify(context.Background(), tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Errorf("ledger of %s: %v", b.tenant, rep.Problems)
		}
		problems, err := audit.CheckTables(context.Background(), tx)
		if len(problems) > 0 {
			t.Errorf("tables of %s differ from the ledger: %v", b.tenant, problems)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLinkRaces: a link's mode is decided again when it is used, under the login's lock, so a link
// made while the login was in this business alone can't reset it once another business uses it.
func TestLinkRaces(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	link := func(l login, x biz) {
		t.Helper()
		if err := l.in(x, func(tx pgx.Tx) error {
			return auth.Link(ctx, tx, l.id, x.person, "operator", ledger.SystemActor)
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("forgot-password link, then the login joins another business: refused", func(t *testing.T) {
		l := newLogin(t, "Iris Interiors (race test)")
		b := l.biz[0]
		code := newLinkFor(t, l, b)
		if h, err := l.linkHolder(b, code, ""); err != nil || h.Mode != auth.ModeChoose {
			t.Fatalf("link holder = %+v, %v; want choose", h, err)
		}
		link(l, newBiz(t, owner, "Jade Jewels (race test)", l.email))
		if _, err := l.useLink(b, code, "taken over password"); !errors.Is(err, auth.ErrLinkedElsewhere) {
			t.Errorf("use link = %v, want ErrLinkedElsewhere", err)
		}
		l.logIn(t) // the password is unchanged
	})

	t.Run("the same while the link is being used: it waits, then is refused", func(t *testing.T) {
		l := newLogin(t, "Kelp Kitchens (race test)")
		b := l.biz[0]
		code := newLinkFor(t, l, b)
		a := newBiz(t, owner, "Lotus Lamps (race test)", l.email)
		joining, err := l.app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer joining.Rollback(ctx)
		if err := db.SetScope(ctx, joining, db.Scope{Tenant: a.tenant}); err != nil {
			t.Fatal(err)
		}
		if err := auth.Link(ctx, joining, l.id, a.person, "operator", ledger.SystemActor); err != nil {
			t.Fatal(err)
		}
		used := make(chan error, 1)
		go func() {
			_, err := l.useLink(b, code, "taken over password")
			used <- err
		}()
		waitForLock(t, l.app, "%FROM accounts WHERE id = $1 FOR UPDATE%")
		if err := joining.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-used; !errors.Is(err, auth.ErrLinkedElsewhere) {
			t.Errorf("use link while the login joined another business = %v, want ErrLinkedElsewhere", err)
		}
		l.logIn(t)
	})

	// operator gives l's email a login linked to the person of x, as `seyal set-password --business`
	// does, in tx (scoped to x), and keeps its id and password in l.
	operator := func(tx pgx.Tx, l *login, x biz) {
		t.Helper()
		var err error
		if l.password, l.id, err = auth.NewPassword(ctx, tx, l.email, x.person); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a link for an email with no login, which then gets one elsewhere: open the link again", func(t *testing.T) {
		l := login{owner: owner, app: app, email: testdb.Email("ravi")}
		b := newBiz(t, owner, "Mango Mills (race test)", l.email)
		code := newLinkFor(t, l, b)
		if h, err := l.linkHolder(b, code, ""); err != nil || h.Mode != auth.ModeChoose {
			t.Fatalf("link holder = %+v, %v; want choose", h, err)
		}
		x := newBiz(t, owner, "Nutmeg Nook (race test)", l.email)
		if err := l.in(x, func(tx pgx.Tx) error { operator(tx, &l, x); return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := l.useLink(b, code, "taken over password"); !errors.Is(err, auth.ErrLinkChanged) {
			t.Errorf("use link = %v, want ErrLinkChanged", err)
		}
		l.logIn(t)
		if h, err := l.linkHolder(b, code, ""); err != nil || h.Mode != auth.ModeJoin {
			t.Errorf("the link opened again = %+v, %v; want join", h, err)
		}
	})

	t.Run("the same while the link is being used: the new login is never set", func(t *testing.T) {
		l := login{owner: owner, app: app, email: testdb.Email("ravi")}
		b := newBiz(t, owner, "Oak Optics (race test)", l.email)
		code := newLinkFor(t, l, b)
		x := newBiz(t, owner, "Pine Paints (race test)", l.email)
		making, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer making.Rollback(ctx)
		if err := db.SetScope(ctx, making, db.Scope{Tenant: x.tenant}); err != nil {
			t.Fatal(err)
		}
		operator(making, &l, x)
		used := make(chan error, 1)
		go func() {
			_, err := l.useLink(b, code, "taken over password")
			used <- err
		}()
		// The link's own new login waits on the operator's, made but not committed yet.
		waitForLock(t, app, "%INSERT INTO accounts%")
		if err := making.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-used; !errors.Is(err, auth.ErrLinkChanged) {
			t.Errorf("use link while the operator made the login = %v, want ErrLinkChanged", err)
		}
		l.logIn(t) // the operator's password
		l.wantEvents(t, b, "person.account_linked", 0)
		l.wantEvents(t, b, "person.link_used", 0)
	})
}

// TestLockOrder: using a link, making one and an admin's email edit lock a person's rows in one
// order (the login, the person, then the rest), so two of them at once never deadlock (Postgres
// would fail one: "deadlock detected"); one waits for the other instead.
func TestLockOrder(t *testing.T) {
	ctx := context.Background()
	// edit changes the person's email in b, as Settings does, and sends its result on the channel.
	edit := func(l login, b biz) chan error {
		done := make(chan error, 1)
		go func() {
			done <- l.in(b, func(tx pgx.Tx) error {
				_, err := people.Update(ctx, tx, b.person, "Asha Moved", testdb.Email("moved"), ledger.SystemActor)
				return err
			})
		}()
		return done
	}
	// hold begins a transaction scoped to b that holds the row the query locks.
	hold := func(t *testing.T, l login, b biz, query string, args ...any) pgx.Tx {
		t.Helper()
		tx, err := l.app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback(ctx) })
		if err := db.SetScope(ctx, tx, db.Scope{Tenant: b.tenant}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	t.Run("a join link used while an admin changes the email", func(t *testing.T) {
		l := newLogin(t, "Quince Quilts (lock test)")
		b := newBiz(t, l.owner, "Rowan Rugs (lock test)", l.email)
		code := newLinkFor(t, l, b)
		// Something else holds the login (e.g. a password change), so the link waits there,
		// before it has locked the person or the link.
		holding := hold(t, l, b, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, l.id)
		used := make(chan error, 1)
		go func() {
			_, err := l.use(b, code, auth.LinkUse{Mode: auth.ModeJoin, Password: l.password})
			used <- err
		}()
		waitForLock(t, l.app, "%FROM accounts WHERE id = $1 FOR UPDATE%")
		edited := edit(l, b)
		select {
		case err := <-edited:
			if err != nil {
				t.Errorf("email edit = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the email edit waited for the link being used")
			if err := holding.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-edited; err != nil {
				t.Errorf("email edit = %v", err)
			}
		}
		holding.Rollback(ctx)
		// The edit cancelled the link, so it no longer works.
		if err := <-used; !errors.Is(err, auth.ErrBadLink) {
			t.Errorf("use link = %v, want ErrBadLink", err)
		}
		checkLedger(t, l, b)
	})

	t.Run("a new link made while an admin changes the email", func(t *testing.T) {
		l := newLogin(t, "Saffron Shades (lock test)")
		b := l.biz[0]
		newLinkFor(t, l, b) // an older link, which both cancel
		// Something else holds b's ledger, so the edit waits there with the person locked.
		holding := hold(t, l, b, `SELECT 1 FROM ledger_heads WHERE tenant_id = current_tenant() FOR UPDATE`)
		edited := edit(l, b)
		waitForLock(t, l.app, "%FROM ledger_heads%FOR UPDATE%")
		made := make(chan error, 1)
		go func() {
			made <- l.in(b, func(tx pgx.Tx) error {
				_, _, err := auth.NewLink(ctx, tx, b.person, ledger.SystemActor)
				return err
			})
		}()
		// The new link waits for the person, before touching the links the edit cancels.
		waitForLock(t, l.app, "%status = 'active' FROM persons%FOR UPDATE%")
		if err := holding.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-edited; err != nil {
			t.Errorf("email edit = %v", err)
		}
		if err := <-made; err != nil {
			t.Errorf("new link = %v", err)
		}
		checkLedger(t, l, b)
	})
}

// TestEmailChanged: an admin's new email for a person unlinks their login in that business only. The
// login, its password and its other businesses stay; nothing links again by email on its own.
func TestEmailChanged(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Olive Oils (email test)", "Plum Pottery (email test)")
	a, b := l.biz[0], l.biz[1]
	token := l.logIn(t)
	for _, x := range []biz{a, b} {
		if _, err := l.checkIn(x, token); err != nil {
			t.Fatal(err)
		}
	}
	pending := newLinkFor(t, l, b)
	update := func(x biz, personID, email string) (u people.Updated, err error) {
		err = l.in(x, func(tx pgx.Tx) error {
			var err error
			u, err = people.Update(ctx, tx, personID, "Asha Moved", email, ledger.SystemActor)
			return err
		})
		return u, err
	}

	u, err := update(b, b.person, testdb.Email("moved"))
	if err != nil || !u.Changed || !u.Unlinked || u.OldEmail != l.email {
		t.Fatalf("update = %+v, %v; want changed and unlinked", u, err)
	}

	t.Run("the login no longer reaches b, and still reaches a", func(t *testing.T) {
		_, err := l.checkIn(b, token)
		wantNoSession(t, "check in b after the email changed", err)
		if _, err := l.checkIn(a, token); err != nil {
			t.Errorf("check in a = %v", err)
		}
		l.logIn(t) // the password is unchanged
		var email string
		if err := l.server(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT email FROM accounts WHERE id = $1", l.id).Scan(&email)
		}); err != nil || email != l.email {
			t.Errorf("the login's email = %q, %v; want it unchanged", email, err)
		}
		var open int
		if err := l.in(b, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE person_id = $1 AND ended_at IS NULL", b.person).Scan(&open)
		}); err != nil || open != 0 {
			t.Errorf("open sessions of the person in b = %d, %v; want 0", open, err)
		}
		l.wantEvents(t, b, "person.account_unlinked", 1)
		l.wantEvents(t, a, "person.account_unlinked", 0)
	})

	t.Run("a link sent to the old email stops working", func(t *testing.T) {
		if _, err := l.linkHolder(b, pending, ""); !errors.Is(err, auth.ErrBadLink) {
			t.Errorf("the old link = %v, want ErrBadLink", err)
		}
	})

	t.Run("the same for someone not set up yet (a mistyped email fixed)", func(t *testing.T) {
		x := addTo(t, l, b, "Not Set Up", testdb.Email("typo"))
		sent := newLinkFor(t, l, biz{b.tenant, x})
		u, err := update(b, x, testdb.Email("fixed"))
		if err != nil || !u.Changed || u.Unlinked || u.OldEmail == "" {
			t.Fatalf("update = %+v, %v; want changed, nothing to unlink", u, err)
		}
		if _, err := l.linkHolder(b, sent, ""); !errors.Is(err, auth.ErrBadLink) {
			t.Errorf("the link sent to the mistyped email = %v, want ErrBadLink", err)
		}
	})

	t.Run("the new email's login isn't linked on its own", func(t *testing.T) {
		victim := newLogin(t, "Quince Quilts (email test)")
		x := addTo(t, l, b, "Second Person", testdb.Email("second"))
		u, err := update(b, x, victim.email)
		if err != nil || u.Unlinked || u.OldEmail == "" {
			t.Fatalf("update = %+v, %v", u, err)
		}
		var linked bool
		if err := l.in(b, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT account_id IS NOT NULL FROM persons WHERE id = $1", x).Scan(&linked)
		}); err != nil || linked {
			t.Errorf("the person with the victim's email is linked = %v, %v", linked, err)
		}
		if m, err := l.linkMode(b, x); err != nil || m != auth.ModeJoin {
			t.Errorf("a link for them = %q, %v; want join (it asks for the victim's password)", m, err)
		}
		_, err = victim.checkIn(b, victim.logIn(t))
		wantNoSession(t, "the victim in b", err)
	})

	// Should Check end an open entry left for another person? Unlinking ends the person's entries,
	// so when the same login session later reaches someone else here, it is a new entry, and the
	// business records that person's logged_in.
	t.Run("the same login session reaching someone new here is a new entry", func(t *testing.T) {
		q := addTo(t, l, b, "Asha Again", l.email)
		if err := l.in(b, func(tx pgx.Tx) error {
			return auth.Link(ctx, tx, l.id, q, "operator", ledger.SystemActor)
		}); err != nil {
			t.Fatal(err)
		}
		s, err := l.checkIn(b, token)
		if err != nil || s.PersonID != q {
			t.Fatalf("check in = %+v, %v; want the new person", s, err)
		}
		var n int
		if err := l.in(b, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE kind = 'person.logged_in' AND subject_id = $1", q).Scan(&n)
		}); err != nil || n != 1 {
			t.Errorf("logged_in for the new person = %d, %v; want 1", n, err)
		}
	})

	t.Run("a rename only unlinks nothing", func(t *testing.T) {
		if u, err := update(a, a.person, l.email); err != nil || u.Unlinked || u.OldEmail != "" {
			t.Errorf("rename only = %+v, %v", u, err)
		}
		if _, err := l.checkIn(a, token); err != nil {
			t.Errorf("check in a after a rename = %v", err)
		}
	})

	checkLedger(t, l, a)
	checkLedger(t, l, b)
}

// TestEmailEditedAndBack: an admin changes a person's email and then puts it back (a typo fixed). In
// between, the login reaches no business at all, yet no other business may set its password: its
// link only joins, with the password the person still knows (D64).
func TestEmailEditedAndBack(t *testing.T) {
	ctx := context.Background()
	l := newLogin(t, "Quartz Quarries (email test)") // the person's login, in a only
	a := l.biz[0]
	token := l.logIn(t)
	update := func(email string) {
		t.Helper()
		if err := l.in(a, func(tx pgx.Tx) error {
			_, err := people.Update(ctx, tx, a.person, "Asha", email, ledger.SystemActor)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	update(testdb.Email("typo"))
	update(l.email)

	b := newBiz(t, l.owner, "Rust Rugs (email test)", l.email) // b's admin adds the same email meanwhile
	code := newLinkFor(t, l, b)
	if h, err := l.linkHolder(b, code, ""); err != nil || h.Mode != auth.ModeJoin {
		t.Errorf("b's link = %+v, %v; want join, never choose", h, err)
	}
	if _, err := l.useLink(b, code, "taken over password"); !errors.Is(err, auth.ErrLinkChanged) {
		t.Errorf("b choosing the password = %v, want ErrLinkChanged", err)
	}
	if _, err := l.checkAccount(token); err != nil {
		t.Errorf("the person's login session = %v; nothing may end it", err)
	}
	l.logIn(t) // the password is unchanged

	// a's new link joins too, with the password the person still knows.
	back := newLinkFor(t, l, a)
	if m, err := l.linkMode(a, a.person); err != nil || m != auth.ModeJoin {
		t.Errorf("a's link = %q, %v; want join", m, err)
	}
	if _, err := l.use(a, back, auth.LinkUse{Mode: auth.ModeJoin, Password: l.password}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.checkIn(a, token); err != nil {
		t.Errorf("the person in a again = %v", err)
	}
	l.wantEvents(t, b, "person.account_linked", 0)
	checkLedger(t, l, a)
	checkLedger(t, l, b)
}

// addTo adds a person to business b and returns their id.
func addTo(t *testing.T, l login, b biz, name, email string) string {
	t.Helper()
	var id string
	if err := l.in(b, func(tx pgx.Tx) error {
		var err error
		id, err = people.Add(context.Background(), tx, name, email, ledger.SystemActor)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
