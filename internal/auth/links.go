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
// forgot theirs) and returns its code, which goes in the link's address. Only the code's SHA-256 is stored.
// The person's older unused links stop working; their current password keeps working until the link
// is used. tx must come from db.WithTenant.
func NewLink(ctx context.Context, tx pgx.Tx, personID string, by ledger.Actor) (code string, expires time.Time, err error) {
	var email string
	var active bool
	err = tx.QueryRow(ctx, `
		SELECT coalesce(login_email, ''), status = 'active' FROM persons
		WHERE tenant_id = current_tenant() AND id::text = $1`, personID).Scan(&email, &active)
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
	replaced, err := tx.Exec(ctx, `
		UPDATE login_links SET cancelled_at = now()
		WHERE tenant_id = current_tenant() AND person_id::text = $1
			AND used_at IS NULL AND cancelled_at IS NULL AND expires_at > now()`, personID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: cancel older links: %w", err)
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
		Payload: map[string]any{"expires_at": expires, "replaced": replaced.RowsAffected()},
	}.By(by))
	return code, expires, err
}

// ErrLinkedElsewhere means a link's email already has a login that is also used in another
// business. A business's link may not set that login's password (D64: no business can take over a
// login another one uses), so for now the link is refused; whoever runs the Seyal server can add
// the person instead.
var ErrLinkedElsewhere = errors.New("this email's Seyal login is also used for another business")

// Holder is the person a link is for, as the link's page shows them.
type Holder struct {
	Name, Email string
	// Elsewhere is true when using the link would be refused with ErrLinkedElsewhere (it is checked
	// again when the link is used). Linked tells the two refusals apart: the person already has that
	// login in this business (they forgot its password, which only the server's operator may reset),
	// or they would be new to it.
	Elsewhere, Linked bool
}

// LinkHolder returns the person a link is for, or ErrBadLink. tx must come from db.WithTenant.
func LinkHolder(ctx context.Context, tx pgx.Tx, code string) (Holder, error) {
	l, err := openLink(ctx, tx, code, "")
	if err != nil {
		return Holder{}, err
	}
	h := Holder{Name: l.name, Email: l.email, Linked: l.accountID != ""}
	h.Elsewhere, err = shared(ctx, tx, l.accountID, l.email)
	return h, err
}

// LinkRefused reports whether a link for this person would be refused now (ErrLinkedElsewhere), and
// whether they already have that login here (see Holder). Settings asks first, so an admin isn't
// given a link that can't work. tx must come from db.WithTenant.
func LinkRefused(ctx context.Context, tx pgx.Tx, personID string) (refused, linked bool, err error) {
	var accountID, email string
	err = tx.QueryRow(ctx, `
		SELECT coalesce(account_id::text, ''), coalesce(login_email, '') FROM persons
		WHERE tenant_id = current_tenant() AND id::text = $1`, personID).Scan(&accountID, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil // NewLink says so
	}
	if err != nil {
		return false, false, err
	}
	refused, err = shared(ctx, tx, accountID, email)
	return refused, accountID != "", err
}

// shared reports whether a person's login (accountID, or if "" the login with their email, if any)
// is also used in another business.
func shared(ctx context.Context, tx pgx.Tx, accountID, email string) (bool, error) {
	if accountID == "" && email != "" {
		var err error
		if accountID, err = findAccount(ctx, tx, email); err != nil {
			return false, err
		}
	}
	if accountID == "" {
		return false, nil
	}
	return linkedElsewhere(ctx, tx, accountID)
}

// UseLink sets the password from a link, ends the link, and starts a login session on this device.
// It returns the session's token for the browser's cookie and when it ends. The login is the one
// already linked to the person, or else the one with the person's email; if there is none, it is
// made. It is linked to the person, and its new password ends its other sessions, as any new
// password does. A login also used in another business is refused (ErrLinkedElsewhere), decided
// again here under the login's lock, as the page may have been opened before it changed.
// tx must come from db.WithTenant.
func UseLink(ctx context.Context, tx pgx.Tx, code, password, userAgent string) (token string, expires time.Time, err error) {
	l, err := openLink(ctx, tx, code, "FOR UPDATE OF l") // two tabs can't both use it
	if err != nil {
		return "", time.Time{}, err
	}
	if err := CheckNew(password); err != nil {
		return "", time.Time{}, err
	}
	accountID := l.accountID
	if accountID == "" {
		if accountID, err = findAccount(ctx, tx, l.email); err != nil {
			return "", time.Time{}, err
		}
	}
	created := false
	if accountID == "" {
		if accountID, created, err = addAccount(ctx, tx, l.email, password); err != nil {
			return "", time.Time{}, err
		}
	}
	if !created {
		if _, err := lockAccount(ctx, tx, accountID); err != nil {
			return "", time.Time{}, err
		}
		elsewhere, err := linkedElsewhere(ctx, tx, accountID)
		if err != nil {
			return "", time.Time{}, err
		}
		if elsewhere {
			return "", time.Time{}, ErrLinkedElsewhere
		}
	}
	me := ledger.PersonActor(l.personID)
	if err := Link(ctx, tx, accountID, l.personID, "link", me); err != nil {
		return "", time.Time{}, err
	}
	// The login is now in this business only, so its new password is recorded here alone.
	if created {
		err = loginSet(ctx, tx, l.personID, l.email, me)
	} else {
		err = setPassword(ctx, tx, accountID, password, false)
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
	}.By(me)); err != nil {
		return "", time.Time{}, err
	}
	return startLogin(ctx, tx, accountID, userAgent)
}

// openedLink is a link that still works, and its person.
type openedLink struct {
	personID, email, name string
	accountID             string // the person's login, or "" if not linked yet
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
