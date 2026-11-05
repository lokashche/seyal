// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// LinkLength is how long a set-your-password link works.
const LinkLength = 24 * time.Hour

// ErrBadLink means a link is unknown, used, replaced by a newer one, expired, or its person is
// disabled. People see one answer for all of these: ask an admin for a new link.
var ErrBadLink = errors.New("this link has expired or was already used")

// NewLink makes a one-time link for a person to set their login's password (a new person, or one who
// forgot theirs), or to add this business to the login they already have (see Mode), and returns its
// code, which goes in the link's address. Only the code's SHA-256 is stored. The person's older
// unused links stop working; their current password keeps working until the link is used.
// tx must come from db.WithTenant.
func NewLink(ctx context.Context, tx pgx.Tx, personID string, by ledger.Actor) (code string, expires time.Time, err error) {
	var email string
	var active bool
	// The person first, before their links and the ledger, as in an email edit (people.Update) and
	// UseLink: two admins acting on one person at once then wait for each other (see UseLink).
	err = tx.QueryRow(ctx, `
		SELECT coalesce(login_email, ''), status = 'active' FROM persons
		WHERE tenant_id = current_tenant() AND id::text = $1 FOR UPDATE`, personID).Scan(&email, &active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", time.Time{}, errors.New("auth: no such person")
	case err != nil:
		return "", time.Time{}, err
	case email == "":
		return "", time.Time{}, errors.New("this person has no login email yet")
	case !active:
		return "", time.Time{}, errors.New("this person is disabled; make them active first")
	}
	replaced, err := cancelLinks(ctx, tx, personID)
	if err != nil {
		return "", time.Time{}, err
	}
	if code, err = newToken(); err != nil {
		return "", time.Time{}, err
	}
	expires = time.Now().Add(LinkLength).UTC().Truncate(time.Second)
	if _, err := tx.Exec(ctx, `
		INSERT INTO login_links (code_sha256, tenant_id, person_id, expires_at)
		VALUES ($1, current_tenant(), $2, $3)`, fingerprint(code), personID, expires); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: make link: %w", err)
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.link_made", SubjectType: "person", SubjectID: personID,
		Payload: map[string]any{"expires_at": expires, "replaced": replaced},
	}.By(by))
	return code, expires, err
}

// cancelLinks stops a person's unused links from working and returns how many there were.
func cancelLinks(ctx context.Context, tx pgx.Tx, personID string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE login_links SET cancelled_at = now()
		WHERE tenant_id = current_tenant() AND person_id::text = $1
			AND used_at IS NULL AND cancelled_at IS NULL AND expires_at > now()`, personID)
	if err != nil {
		return 0, fmt.Errorf("auth: cancel links: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Mode is what a link's page asks of the person who opens it. No business may take over a login
// that isn't used here alone (D64), so a link sets a password only for a new login, or for the
// person's own login when no other business uses it.
type Mode string

const (
	// ModeChoose: choose a password. The email has no login yet, or the person's own login is used
	// in this business only, so this business may set its password.
	ModeChoose Mode = "choose"
	// ModeJoin: the email already has a login, and this person isn't linked to it: it is used in
	// another business, or in none any more (e.g. an email edit unlinked it). The link only adds this
	// business to the login: its holder gives the login's current password, or taps once when
	// already logged in with it. The password never changes.
	ModeJoin Mode = "join"
	// ModeRefused: the person already has that login here and it is also used in another business
	// (they forgot its password). Only whoever runs the server may reset it (seyal set-password).
	ModeRefused Mode = "refused"
)

// ErrLinkedElsewhere means a link is for a person who already has a login that is also used in
// another business (ModeRefused): a business's link may not change that login's password.
var ErrLinkedElsewhere = errors.New("this person's Seyal login is also used for another business")

// ErrLinkChanged means the link's page was shown with another mode than the link has now (e.g. the
// login was added to another business meanwhile). Opening the link again shows what to do.
var ErrLinkChanged = errors.New("this link changed after its page was opened; please open the link again")

// ErrOtherLogin means a join link was used in a browser logged in with a different login.
var ErrOtherLogin = errors.New("this link is for another login; log out first")

// Holder is the person a link is for, as the link's page shows them.
type Holder struct {
	Name, Email string
	Mode        Mode // decided again when the link is used, as it may change meanwhile
	// For ModeJoin, who this browser is logged in as: Same is the link's own login (one tap adds the
	// business), Other is a different login (log out first). Both false: not logged in.
	Same, Other bool
}

// LinkHolder returns the person a link is for, or ErrBadLink. token is the browser's login cookie
// ("" if none). tx must come from db.WithTenant.
func LinkHolder(ctx context.Context, tx pgx.Tx, code, token string) (Holder, error) {
	l, err := openLink(ctx, tx, code, "")
	if err != nil {
		return Holder{}, err
	}
	h := Holder{Name: l.name, Email: l.email}
	accountID, err := l.login(ctx, tx)
	if err != nil {
		return Holder{}, err
	}
	if h.Mode, err = linkMode(ctx, tx, l, accountID); err != nil || h.Mode != ModeJoin {
		return h, err
	}
	h.Same, h.Other, err = whoIsHere(ctx, tx, token, accountID)
	return h, err
}

// LinkModeOf is the mode a new link for this person would have now. Settings asks first, so an admin
// isn't given a link that can't work (ModeRefused). tx must come from db.WithTenant.
func LinkModeOf(ctx context.Context, tx pgx.Tx, personID string) (Mode, error) {
	var l openedLink
	err := tx.QueryRow(ctx, `
		SELECT id::text, coalesce(login_email, ''), coalesce(account_id::text, '') FROM persons
		WHERE tenant_id = current_tenant() AND id::text = $1`, personID).Scan(&l.personID, &l.email, &l.accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModeChoose, nil // NewLink says so
	}
	if err != nil {
		return "", err
	}
	accountID, err := l.login(ctx, tx)
	if err != nil {
		return "", err
	}
	return linkMode(ctx, tx, l, accountID)
}

// linkMode decides a link's mode from its person and their login (accountID, "" if none).
func linkMode(ctx context.Context, tx pgx.Tx, l openedLink, accountID string) (Mode, error) {
	switch {
	case accountID == "":
		return ModeChoose, nil
	case l.accountID == "":
		// The email has a login, but not as this person's. Even one in no business may be someone
		// else's (e.g. unlinked by an email edit a moment ago), so only its password shows it's theirs.
		return ModeJoin, nil
	}
	elsewhere, err := linkedElsewhere(ctx, tx, accountID)
	switch {
	case err != nil:
		return "", err
	case elsewhere:
		return ModeRefused, nil
	}
	return ModeChoose, nil
}

// whoIsHere compares the browser's login (token, "" if none) with accountID: the same login, another
// one, or neither (not logged in, or its session ended).
func whoIsHere(ctx context.Context, tx pgx.Tx, token, accountID string) (same, other bool, err error) {
	a, err := CheckAccount(ctx, tx, token)
	if errors.Is(err, ErrNoSession) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return a.ID == accountID, a.ID != accountID, nil
}

// LinkUse is what a link's page sends back.
type LinkUse struct {
	Mode Mode // the mode the page was shown with; if the link's mode is different now, ErrLinkChanged
	// Password is the new password (ModeChoose), or the login's current one (ModeJoin, unless the
	// browser is already logged in with it).
	Password  string
	Token     string // the browser's login cookie, "" if none
	UserAgent string
}

// UseLink uses a link, ends it, and returns a login session for this device: the token for the
// browser's cookie and when it ends. The login is the one already linked to the person, or else the
// one with the person's email. The mode is decided again here, under the link's and the login's
// locks; if it isn't the one the page was shown with, nothing changes (ErrLinkChanged).
//
// Locks are taken in one order: the login, then the person, and only then the link and the ledger.
// Everything else that changes a person's login or links locks the login before the person (Link)
// and the person before the rest (NewLink, an admin's email edit in people.Update). So two of them
// never wait for each other in a circle, which Postgres would end by failing one of them ("deadlock
// detected"): one just waits for the other to finish.
//
//   - ModeChoose sets the password (making the login if there is none) and links the login to the
//     person. Like any new password, it ends the login's other sessions.
//   - ModeJoin links the login to the person after checking its current password (ErrWrongPassword
//     if wrong), or at once if the browser is already logged in with it; a browser logged in with
//     another login is refused (ErrOtherLogin). The password never changes and no other session
//     ends. One tap keeps the browser's session; a password starts a new one.
//   - ModeRefused is always refused (ErrLinkedElsewhere).
//
// tx must come from db.WithTenant.
func UseLink(ctx context.Context, tx pgx.Tx, code string, u LinkUse) (token string, expires time.Time, err error) {
	// A first look, without locks, to learn which login and person to lock.
	l, err := openLink(ctx, tx, code, "")
	if err != nil {
		return "", time.Time{}, err
	}
	accountID, err := l.login(ctx, tx)
	if err != nil {
		return "", time.Time{}, err
	}
	if accountID != "" {
		// Changes to the login (a new password, a link to another business) now wait for this
		// transaction, so the mode decided next stays true until it ends.
		if _, err := lockAccount(ctx, tx, accountID); err != nil {
			return "", time.Time{}, err
		}
	}
	if err := lockPerson(ctx, tx, l.personID); err != nil {
		return "", time.Time{}, err
	}
	// Look again under the locks: the link may have been used, replaced or cancelled meanwhile
	// (ErrBadLink), or the person's email or login changed.
	if l, err = openLink(ctx, tx, code, "FOR UPDATE OF l"); err != nil { // two tabs can't both use it
		return "", time.Time{}, err
	}
	switch now, err := l.login(ctx, tx); {
	case err != nil:
		return "", time.Time{}, err
	case now != accountID:
		return "", time.Time{}, ErrLinkChanged // not the login locked above; opening the link again shows what to do
	}
	mode, err := linkMode(ctx, tx, l, accountID)
	switch {
	case err != nil:
		return "", time.Time{}, err
	case mode == ModeRefused:
		return "", time.Time{}, ErrLinkedElsewhere
	case mode != u.Mode:
		return "", time.Time{}, ErrLinkChanged
	}
	if mode == ModeJoin {
		token, expires, err = join(ctx, tx, l, accountID, u)
	} else {
		token, expires, err = choose(ctx, tx, l, accountID, u)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE login_links SET used_at = now() WHERE tenant_id = current_tenant() AND code_sha256 = $1`,
		fingerprint(code)); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: use link: %w", err)
	}
	if _, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.link_used", SubjectType: "person", SubjectID: l.personID,
	}.By(ledger.PersonActor(l.personID))); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// choose is UseLink in ModeChoose: the login (made now if accountID is "") gets the new password and
// is linked to the person.
func choose(ctx context.Context, tx pgx.Tx, l openedLink, accountID string, u LinkUse) (string, time.Time, error) {
	if err := CheckNew(u.Password); err != nil {
		return "", time.Time{}, err
	}
	created := false
	if accountID == "" {
		var err error
		if accountID, created, err = addAccount(ctx, tx, l.email, u.Password); err != nil {
			return "", time.Time{}, err
		}
		if !created {
			// Someone made a login for this email meanwhile (e.g. the operator, for another
			// business). It isn't this person's, so the link may only join it now (linkMode).
			return "", time.Time{}, ErrLinkChanged
		}
	}
	me := ledger.PersonActor(l.personID)
	if err := Link(ctx, tx, accountID, l.personID, "link", me); err != nil {
		return "", time.Time{}, err
	}
	// The login is now in this business only, so its new password is recorded here alone.
	var err error
	if created {
		err = loginSet(ctx, tx, l.personID, l.email, me)
	} else {
		err = setPassword(ctx, tx, accountID, u.Password, false)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return startLogin(ctx, tx, accountID, u.UserAgent)
}

// join is UseLink in ModeJoin: the holder shows the login is theirs (its session in this browser, or
// its password), then it is linked to the person.
func join(ctx context.Context, tx pgx.Tx, l openedLink, accountID string, u LinkUse) (string, time.Time, error) {
	here, err := CheckAccount(ctx, tx, u.Token)
	switch {
	case errors.Is(err, ErrNoSession): // not logged in here: the password decides
		if u.Password == "" {
			return "", time.Time{}, ErrWrongPassword // nothing to check (e.g. the browser's session ended)
		}
		var id string
		var ok bool
		err = tx.QueryRow(ctx, `SELECT id::text, ok FROM account_check($1, $2)`, l.email, u.Password).Scan(&id, &ok)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || (err == nil && (!ok || id != accountID)):
			return "", time.Time{}, ErrWrongPassword
		case err != nil:
			return "", time.Time{}, err
		}
	case err != nil:
		return "", time.Time{}, err
	case here.ID != accountID:
		return "", time.Time{}, ErrOtherLogin
	}
	if err := Link(ctx, tx, accountID, l.personID, "link", ledger.PersonActor(l.personID)); err != nil {
		return "", time.Time{}, err
	}
	if here.ID == accountID {
		return u.Token, here.ExpiresAt, nil // carry on with the session this browser has
	}
	return startLogin(ctx, tx, accountID, u.UserAgent)
}

// EmailChanged is what a new email means for a person's login, in the current business; an admin
// has just changed it (people.Update). The person's unused links stop working, as one sent to the
// old address must not reach the new one. If the person had a login, it no longer reaches them here
// (unlinked): their sessions here end and the business records person.account_unlinked. The login
// itself (its email, its password, its other businesses) stays as it was. Nothing is ever linked
// again by email on its own: the person needs a new link for the new email.
// tx must come from db.WithTenant.
func EmailChanged(ctx context.Context, tx pgx.Tx, personID string, by ledger.Actor) (unlinked bool, err error) {
	cancelled, err := cancelLinks(ctx, tx, personID)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE persons SET account_id = NULL
		WHERE tenant_id = current_tenant() AND id::text = $1 AND account_id IS NOT NULL`, personID)
	if err != nil {
		return false, fmt.Errorf("auth: unlink: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := endSessions(ctx, tx, personID); err != nil {
		return false, err
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.account_unlinked", SubjectType: "person", SubjectID: personID,
		Payload: map[string]any{"why": "email changed", "links_cancelled": cancelled},
	}.By(by))
	return err == nil, err
}

// openedLink is a link that still works, and its person.
type openedLink struct {
	personID, email, name string
	accountID             string // the person's login, or "" if not linked yet
}

// login is the login a link works on: the person's own, or else the one with their email ("" if
// there is none).
func (l openedLink) login(ctx context.Context, tx pgx.Tx) (string, error) {
	if l.accountID != "" || l.email == "" {
		return l.accountID, nil
	}
	return FindAccount(ctx, tx, l.email)
}

// lockPerson locks a person's row until the transaction ends.
func lockPerson(ctx context.Context, tx pgx.Tx, personID string) error {
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM persons WHERE tenant_id = current_tenant() AND id::text = $1 FOR UPDATE`, personID); err != nil {
		return fmt.Errorf("auth: lock person: %w", err)
	}
	return nil
}

// openLink finds a link that still works. lock is "" or a locking clause for the query.
func openLink(ctx context.Context, tx pgx.Tx, code, lock string) (l openedLink, err error) {
	err = tx.QueryRow(ctx, `
		SELECT l.person_id::text, p.login_email, p.display_name, coalesce(p.account_id::text, '')
		FROM login_links l JOIN persons p ON p.tenant_id = l.tenant_id AND p.id = l.person_id
		WHERE l.tenant_id = current_tenant() AND l.code_sha256 = $1
			AND l.used_at IS NULL AND l.cancelled_at IS NULL AND l.expires_at > now()
			AND p.status = 'active' AND p.login_email IS NOT NULL `+lock, fingerprint(code)).
		Scan(&l.personID, &l.email, &l.name, &l.accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return openedLink{}, ErrBadLink
	}
	return l, err
}
