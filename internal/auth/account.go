// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

// One login for a person in any number of businesses (D64, migrations 00015 and 00016). A login
// (accounts) is an email and one password for the whole server; in each business it reaches one
// person (persons.account_id). Passwords are checked only inside the database, by account_check():
// Go makes bcrypt hashes for new passwords but never reads one back.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"seyal/internal/db"
	"seyal/internal/ledger"
)

// Account is a logged-in login, before any business is chosen.
type Account struct {
	ID        string
	Email     string
	ExpiresAt time.Time
	// MustChange is true while the login's password is a temporary one: its holder chooses their own
	// before anything else.
	MustChange bool
}

// Business is one business a login belongs to, and the person it reaches there.
type Business struct {
	TenantID string
	Slug     string
	Name     string
	PersonID string
	Active   bool // the person may use the business; a disabled one may be enabled again
}

// LogIn checks an email and password and starts a login session, for every business the login
// belongs to. It returns the token for the browser's cookie. No ledger event is written: no business
// has been opened yet (Check records that). tx must come from db.WithServer.
func LogIn(ctx context.Context, tx pgx.Tx, email, password, userAgent string) (token string, a Account, err error) {
	a.Email = strings.ToLower(strings.TrimSpace(email))
	// First wait for any password change of this login still in progress (it holds the row FOR
	// UPDATE). account_check, a new statement, then sees the new hash, so an old password can't start
	// a session after the change has ended them all.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE email = $1 FOR SHARE`, a.Email); err != nil {
		return "", Account{}, fmt.Errorf("auth: log in: %w", err)
	}
	// An unknown email gives no row and a wrong password ok = false; both take one bcrypt check
	// inside account_check, so the answer and its timing are the same.
	var ok bool
	err = tx.QueryRow(ctx, `SELECT id::text, password_temporary, ok FROM account_check($1, $2)`, a.Email, password).
		Scan(&a.ID, &a.MustChange, &ok)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !ok):
		return "", Account{}, ErrBadLogin
	case err != nil:
		return "", Account{}, err
	}
	if token, a.ExpiresAt, err = startLogin(ctx, tx, a.ID, userAgent); err != nil {
		return "", Account{}, err
	}
	return token, a, nil
}

// startLogin starts a login session for accountID and returns its token and end.
func startLogin(ctx context.Context, tx pgx.Tx, accountID, userAgent string) (token string, expires time.Time, err error) {
	if token, err = newToken(); err != nil {
		return "", time.Time{}, err
	}
	expires = time.Now().Add(SessionLength).UTC().Truncate(time.Second)
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_sessions (token_sha256, account_id, expires_at, user_agent) VALUES ($1, $2, $3, $4)`,
		fingerprint(token), accountID, expires, cleanAgent(userAgent)); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: start login: %w", err)
	}
	return token, expires, nil
}

// CheckAccount returns the login for a token from a cookie, or ErrNoSession. It is for pages
// outside any business (choosing one); inside a business use Check. Any transaction will do.
func CheckAccount(ctx context.Context, tx pgx.Tx, token string) (Account, error) {
	if token == "" {
		return Account{}, ErrNoSession
	}
	var a Account
	err := tx.QueryRow(ctx, `
		SELECT a.id::text, a.email, s.expires_at, a.password_temporary
		FROM account_sessions s JOIN accounts a ON a.id = s.account_id
		WHERE s.token_sha256 = $1 AND s.ended_at IS NULL AND s.expires_at > now()`,
		fingerprint(token)).Scan(&a.ID, &a.Email, &a.ExpiresAt, &a.MustChange)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNoSession
	}
	return a, err
}

// Check returns the session for a login token inside the current business, or ErrNoSession if the
// token doesn't work or the login has no active person here (not a member, disabled, or unlinked:
// refused in this business at once, still fine in the others).
//
// The first request of a login session in a business records an "entry" (a sessions row tied to
// the login session) and person.logged_in, in this business's ledger only. Two requests arriving at
// once give one entry and one event: the second waits on the first's row and then adds nothing.
// An entry that was ended (the person's sessions were ended here, e.g. on being unlinked) no longer
// counts: the next request records a new one. tx must come from db.WithTenant.
func Check(ctx context.Context, tx pgx.Tx, token, userAgent string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	login := fingerprint(token)
	var s Session
	var entered bool
	err := tx.QueryRow(ctx, `
		SELECT p.id::text, p.display_name, a.id::text, a.email, s.expires_at, a.password_temporary,
			EXISTS (SELECT 1 FROM sessions e WHERE e.tenant_id = current_tenant()
				AND e.account_session = s.token_sha256 AND e.ended_at IS NULL)
		FROM account_sessions s
			JOIN accounts a ON a.id = s.account_id
			JOIN persons p ON p.account_id = a.id AND p.tenant_id = current_tenant() AND p.status = 'active'
		WHERE s.token_sha256 = $1 AND s.ended_at IS NULL AND s.expires_at > now()`, login).
		Scan(&s.PersonID, &s.Name, &s.AccountID, &s.Email, &s.ExpiresAt, &s.MustChange, &entered)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	if !entered {
		// The entry's own key is the hash of a token nobody is given, so no cookie can open it as
		// an old per-business session.
		key, err := newToken()
		if err != nil {
			return Session{}, err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO sessions (token_sha256, tenant_id, person_id, expires_at, user_agent, account_session)
			VALUES ($1, current_tenant(), $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			fingerprint(key), s.PersonID, s.ExpiresAt, cleanAgent(userAgent), login)
		if err != nil {
			return Session{}, fmt.Errorf("auth: enter business: %w", err)
		}
		if tag.RowsAffected() == 1 {
			if _, err := ledger.Append(ctx, tx, ledger.Event{
				Kind: "person.logged_in", SubjectType: "person", SubjectID: s.PersonID,
			}.By(ledger.PersonActor(s.PersonID))); err != nil {
				return Session{}, err
			}
		}
	}
	s.Roles, err = roles(ctx, tx, s.PersonID)
	return s, err
}

// Businesses lists the businesses a login belongs to, by name, with disabled memberships too (see
// Business.Active). It briefly switches tx to the login's own scope, where row-level security shows
// only its own people, and then puts the scope back. Any transaction will do.
func Businesses(ctx context.Context, tx pgx.Tx, accountID string) ([]Business, error) {
	if accountID == "" {
		return nil, errors.New("auth: empty login id")
	}
	was, err := db.CurrentScope(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := db.SetScope(ctx, tx, db.Scope{Account: accountID}); err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `
		SELECT t.id::text, t.slug, t.name, p.id::text, p.status = 'active'
		FROM persons p JOIN tenants t ON t.id = p.tenant_id
		WHERE p.account_id = current_account()
		ORDER BY t.name, t.id`)
	bs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Business])
	if err != nil {
		return nil, fmt.Errorf("auth: list businesses: %w", err)
	}
	return bs, db.SetScope(ctx, tx, was)
}

// eachBusiness runs fn once in every business the login belongs to (of any status), with tx scoped
// to that business, and then puts the scope back. It is how one login's change (a new password,
// logging out) is written into each business's own ledger, all in one transaction: every chain gets
// it or none does. Businesses are taken in tenant-id order, the order every such transaction locks
// their ledger heads in, so two of them can't wait for each other in a circle.
func eachBusiness(ctx context.Context, tx pgx.Tx, accountID string, fn func(Business) error) error {
	bs, err := Businesses(ctx, tx, accountID)
	if err != nil {
		return err
	}
	slices.SortFunc(bs, func(a, b Business) int { return strings.Compare(a.TenantID, b.TenantID) })
	was, err := db.CurrentScope(ctx, tx)
	if err != nil {
		return err
	}
	for _, b := range bs {
		if err := db.SetScope(ctx, tx, db.Scope{Tenant: b.TenantID}); err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
	}
	return db.SetScope(ctx, tx, was)
}

// linkedElsewhere reports whether the login reaches a person (of any status, since a disabled one
// may be enabled again) in a business other than the current one. tx must come from db.WithTenant.
func linkedElsewhere(ctx context.Context, tx pgx.Tx, accountID string) (bool, error) {
	here, err := db.CurrentScope(ctx, tx)
	if err != nil {
		return false, err
	}
	bs, err := Businesses(ctx, tx, accountID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(bs, func(b Business) bool { return b.TenantID != here.Tenant }), nil
}

// LogOut ends a login session, in every business. Each business it was used in records
// person.logged_out; the others record nothing. Any transaction will do.
//
// FOR ONE RELEASE (D64): it also ends the login's people's old per-business sessions, in every
// business. Otherwise an old cookie for another business address, still in this browser, would be
// swapped for a new login session on the next visit (Upgrade), and log the device straight back in.
func LogOut(ctx context.Context, tx pgx.Tx, token string) error {
	login := fingerprint(token)
	var accountID string
	err := tx.QueryRow(ctx, `
		UPDATE account_sessions SET ended_at = now() WHERE token_sha256 = $1 AND ended_at IS NULL
		RETURNING account_id::text`, login).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already logged out
	}
	if err != nil {
		return err
	}
	return eachBusiness(ctx, tx, accountID, func(b Business) error {
		var personID string
		err := tx.QueryRow(ctx, `
			UPDATE sessions SET ended_at = now()
			WHERE tenant_id = current_tenant() AND account_session = $1 AND ended_at IS NULL
			RETURNING person_id::text`, login).Scan(&personID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) { // no row: never opened this business with this session
			return err
		}
		old, err := tx.Exec(ctx, `
			UPDATE sessions SET ended_at = now()
			WHERE tenant_id = current_tenant() AND person_id::text = $1 AND account_session IS NULL AND ended_at IS NULL`,
			b.PersonID)
		if err != nil {
			return fmt.Errorf("auth: end old sessions: %w", err)
		}
		if personID == "" && old.RowsAffected() == 0 {
			return nil // nothing was open here
		}
		if personID == "" {
			personID = b.PersonID
		}
		_, err = ledger.Append(ctx, tx, ledger.Event{
			Kind: "person.logged_out", SubjectType: "person", SubjectID: personID,
		}.By(ledger.PersonActor(personID)))
		return err
	})
}

// LogOutEverywhere ends all of a login's sessions, on every device and in every business (e.g. a
// phone was lost). Every business it belongs to records person.logged_out_everywhere with the number
// of that business's sessions it ended. Any transaction will do.
func LogOutEverywhere(ctx context.Context, tx pgx.Tx, accountID string) error {
	if err := endLogins(ctx, tx, accountID); err != nil {
		return err
	}
	return eachBusiness(ctx, tx, accountID, func(b Business) error {
		n, err := endSessions(ctx, tx, b.PersonID)
		if err != nil {
			return err
		}
		_, err = ledger.Append(ctx, tx, ledger.Event{
			Kind: "person.logged_out_everywhere", SubjectType: "person", SubjectID: b.PersonID,
			Payload: map[string]int64{"sessions": n},
		}.By(ledger.PersonActor(b.PersonID)))
		return err
	})
}

// endLogins ends every open session of a login.
func endLogins(ctx context.Context, tx pgx.Tx, accountID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE account_sessions SET ended_at = now() WHERE account_id = $1 AND ended_at IS NULL`, accountID); err != nil {
		return fmt.Errorf("auth: end logins: %w", err)
	}
	return nil
}

// endSessions ends a person's open sessions in the current business (entries and old-style ones)
// and returns how many.
func endSessions(ctx context.Context, tx pgx.Tx, personID string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE sessions SET ended_at = now()
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND ended_at IS NULL`, personID)
	if err != nil {
		return 0, fmt.Errorf("auth: end sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// setPassword gives a login a new (permanent) password. Like any new password it logs the login out
// everywhere: if the old one leaked, its sessions end too. Every business the login belongs to
// records person.login_set with the login's email, never the password or its hash; the actor is
// that business's own person, or Seyal itself when system is true (the operator's command, the demo
// seed).
func setPassword(ctx context.Context, tx pgx.Tx, accountID, password string, system bool) error {
	if err := CheckNew(password); err != nil {
		return err
	}
	email, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET password_hash = $2, password_temporary = false WHERE id = $1`,
		accountID, string(hash)); err != nil {
		return fmt.Errorf("auth: set password: %w", err)
	}
	if err := endLogins(ctx, tx, accountID); err != nil {
		return err
	}
	return eachBusiness(ctx, tx, accountID, func(b Business) error {
		if _, err := endSessions(ctx, tx, b.PersonID); err != nil {
			return err
		}
		by := ledger.PersonActor(b.PersonID)
		if system {
			by = ledger.SystemActor
		}
		return loginSet(ctx, tx, b.PersonID, email, by)
	})
}

// loginSet records in the current business that a person's login got a new password: the email
// only, never the password or its hash.
func loginSet(ctx context.Context, tx pgx.Tx, personID, email string, by ledger.Actor) error {
	_, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.login_set", SubjectType: "person", SubjectID: personID,
		Payload: map[string]string{"email": email},
	}.By(by))
	return err
}

// lockAccount locks a login's row until the transaction ends and returns its email. Changes to one
// login (its password, a new link to it) then happen one after another.
func lockAccount(ctx context.Context, tx pgx.Tx, accountID string) (email string, err error) {
	err = tx.QueryRow(ctx, `SELECT email FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("auth: no such login")
	}
	return email, err
}

// ChangePassword lets a login's holder replace its password, after giving the current one.
// It ends all the login's sessions in every business; the caller logs them in again on this device.
// Any transaction will do.
func ChangePassword(ctx context.Context, tx pgx.Tx, accountID, current, next string) error {
	email, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return err
	}
	var id string
	var ok bool
	err = tx.QueryRow(ctx, `SELECT id::text, ok FROM account_check($1, $2)`, email, current).Scan(&id, &ok)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && (!ok || id != accountID)):
		return ErrWrongPassword
	case err != nil:
		return err
	}
	if next == current {
		return ErrSamePassword
	}
	return setPassword(ctx, tx, accountID, next, false)
}

// NewPassword gives the login with this email a new random password, making the login if
// there is none, and returns the password (shown once, never stored) and the login's id. It is for
// Seyal itself only (the operator's `seyal set-password`, the demo seed), so the password is never
// a temporary one and the ledger records Seyal as the actor. Passwords are 16 characters from A-Z and
// 2-7, so there's no 0/O or 1/l to mix up when reading one out.
//
// With a personID (a person of the current business, tx from db.WithTenant), the login is first
// linked to that person (Link, "operator"), so this business records the new password too.
// Otherwise any transaction will do.
func NewPassword(ctx context.Context, tx pgx.Tx, email, personID string) (password, accountID string, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return "", "", errors.New("that doesn't look like an email address")
	}
	password = rand.Text()[:16]
	if accountID, err = findAccount(ctx, tx, email); err != nil {
		return "", "", err
	}
	created := false
	if accountID == "" {
		if accountID, created, err = addAccount(ctx, tx, email, password); err != nil {
			return "", "", err
		}
	}
	if personID != "" {
		if err := Link(ctx, tx, accountID, personID, "operator", ledger.SystemActor); err != nil {
			return "", "", err
		}
	}
	switch {
	case !created:
		err = setPassword(ctx, tx, accountID, password, true)
	case personID != "":
		// A new login reaches only this person: no other sessions to end, no other ledger to write.
		err = loginSet(ctx, tx, personID, email, ledger.SystemActor)
	}
	if err != nil {
		return "", "", err
	}
	return password, accountID, nil
}

// findAccount returns the id of the login with this email, or "" if there is none.
func findAccount(ctx context.Context, tx pgx.Tx, email string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM accounts WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// addAccount makes a login with this email and password and returns its id. If another
// transaction made one for the email first, it returns that login's id and created = false. It is a
// plain INSERT inside a savepoint, not ON CONFLICT, which would need to read password hashes; the
// savepoint keeps tx usable after the clash.
func addAccount(ctx context.Context, tx pgx.Tx, email, password string) (id string, created bool, err error) {
	if err := CheckNew(password); err != nil {
		return "", false, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", false, err
	}
	err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
		return sp.QueryRow(ctx, `
			INSERT INTO accounts (email, password_hash, password_temporary) VALUES ($1, $2, false)
			RETURNING id::text`, email, string(hash)).Scan(&id)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: made meanwhile
		id, err = findAccount(ctx, tx, email)
		return id, false, err
	}
	if err != nil {
		return "", false, fmt.Errorf("auth: add login: %w", err)
	}
	return id, true, nil
}

// Link makes a login reach a person of the current business, without touching its password, and
// records person.account_linked with how ("link" for a set-your-password link, "operator" for the
// operator's command). The person must be active and have the login's email; a person already
// reached by another login is never moved to this one, and a business has at most one person per
// login. The person's open sessions here end. Linking twice changes nothing. tx must come from
// db.WithTenant.
func Link(ctx context.Context, tx pgx.Tx, accountID, personID, how string, by ledger.Actor) error {
	email, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return err
	}
	var current, personEmail string
	var active bool
	err = tx.QueryRow(ctx, `
		SELECT coalesce(account_id::text, ''), coalesce(login_email, ''), status = 'active' FROM persons
		WHERE tenant_id = current_tenant() AND id::text = $1 FOR UPDATE`, personID).Scan(&current, &personEmail, &active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errors.New("auth: no such person")
	case err != nil:
		return err
	case current == accountID:
		return nil
	case current != "":
		return errors.New("this person already has another login")
	case !active:
		return errors.New("this person is disabled; make them active first")
	case personEmail != email:
		return errors.New("this person's email isn't the login's email")
	}
	var taken bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM persons WHERE tenant_id = current_tenant() AND account_id = $1)`,
		accountID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return errors.New("this login already reaches someone else in this business")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE persons SET account_id = $1 WHERE tenant_id = current_tenant() AND id::text = $2`,
		accountID, personID); err != nil {
		return fmt.Errorf("auth: link: %w", err)
	}
	// The person's sessions here from before the link end, so none of them (an old per-business one,
	// maybe someone else's) can be swapped for a session of the login and reach its other businesses.
	if _, err := endSessions(ctx, tx, personID); err != nil {
		return err
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.account_linked", SubjectType: "person", SubjectID: personID,
		Payload: map[string]string{"how": how},
	}.By(by))
	return err
}

// cleanAgent keeps a browser's description short and valid text, as the sessions tables require.
func cleanAgent(userAgent string) string {
	if len(userAgent) > 500 {
		userAgent = userAgent[:500]
	}
	return strings.ToValidUTF8(userAgent, "")
}
