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

// NewLink makes a one-time link for a person to set their password (a new person, or one who forgot
// theirs) and returns its code, which goes in the link's address. Only the code's SHA-256 is stored.
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

// LinkHolder returns the name and login email of the person a link is for, or ErrBadLink.
// tx must come from db.WithTenant.
func LinkHolder(ctx context.Context, tx pgx.Tx, code string) (name, email string, err error) {
	_, email, name, err = openLink(ctx, tx, code, "")
	return name, email, err
}

// UseLink sets the password of the person a link is for, ends the link, and logs them in on this
// device (like any new password, it ends their other sessions). tx must come from db.WithTenant.
func UseLink(ctx context.Context, tx pgx.Tx, code, password, userAgent string) (token string, s Session, err error) {
	personID, email, _, err := openLink(ctx, tx, code, "FOR UPDATE OF l") // two tabs can't both use it
	if err != nil {
		return "", Session{}, err
	}
	if err := CheckNew(password); err != nil {
		return "", Session{}, err
	}
	if err := SetPassword(ctx, tx, personID, email, password, ledger.PersonActor(personID)); err != nil {
		return "", Session{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE login_links SET used_at = now() WHERE tenant_id = current_tenant() AND code_sha256 = $1`,
		fingerprint(code)); err != nil {
		return "", Session{}, fmt.Errorf("auth: use link: %w", err)
	}
	if _, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.link_used", SubjectType: "person", SubjectID: personID,
	}.By(ledger.PersonActor(personID))); err != nil {
		return "", Session{}, err
	}
	return Login(ctx, tx, email, password, userAgent)
}

// openLink finds a link that still works. lock is "" or a locking clause for the query.
func openLink(ctx context.Context, tx pgx.Tx, code, lock string) (personID, email, name string, err error) {
	err = tx.QueryRow(ctx, `
		SELECT l.person_id::text, p.login_email, p.display_name
		FROM login_links l JOIN persons p ON p.tenant_id = l.tenant_id AND p.id = l.person_id
		WHERE l.tenant_id = current_tenant() AND l.code_sha256 = $1
			AND l.used_at IS NULL AND l.cancelled_at IS NULL AND l.expires_at > now()
			AND p.status = 'active' AND p.login_email IS NOT NULL `+lock, fingerprint(code)).Scan(&personID, &email, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrBadLink
	}
	return personID, email, name, err
}
