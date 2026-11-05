// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

// FOR ONE RELEASE ONLY (D64): remove this file, its test and its use in the server together with the
// migration that drops persons.password_hash (00017).
//
// Before one login across businesses, each business address had its own cookie and session (a
// sessions row with no login session). Staff still logged in that way stay logged in: their old
// session is swapped once for a login session.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Upgrade swaps a valid old per-business session for a login session and returns the new token for
// the browser's cookie and when it ends. The old session is ended. Only a person who is active and
// linked to a login is upgraded; anyone else gets ErrNoSession and logs in again. A login session
// or a business entry can never be passed off as an old session here.
//
// No ledger event is written for the old session's end: the next Check records person.logged_in
// for the new one, in this business. tx must come from db.WithTenant.
func Upgrade(ctx context.Context, tx pgx.Tx, oldToken, userAgent string) (token string, expires time.Time, err error) {
	if oldToken == "" {
		return "", time.Time{}, ErrNoSession
	}
	var accountID string
	err = tx.QueryRow(ctx, `
		UPDATE sessions s SET ended_at = now()
		FROM persons p
		WHERE s.tenant_id = current_tenant() AND s.token_sha256 = $1 AND s.account_session IS NULL
			AND s.ended_at IS NULL AND s.expires_at > now()
			AND p.tenant_id = s.tenant_id AND p.id = s.person_id AND p.status = 'active' AND p.account_id IS NOT NULL
		RETURNING p.account_id::text`, fingerprint(oldToken)).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, ErrNoSession
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: upgrade old session: %w", err)
	}
	return startLogin(ctx, tx, accountID, userAgent)
}
