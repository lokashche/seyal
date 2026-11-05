// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth handles logins: passwords (stored only as bcrypt hashes, checked only inside the
// database) and sessions (random tokens whose SHA-256 is stored). One login (an email and one
// password) reaches a person in each business it belongs to; see account.go.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	// SessionLength is how long a login lasts before logging in again.
	SessionLength = 14 * 24 * time.Hour
	// MinPassword is the shortest password accepted, in characters. bcrypt reads at most 72 bytes.
	MinPassword = 10
	maxPassword = 72
)

var (
	// ErrBadLogin is the only error a failed login gives, whether the email or the password was
	// wrong, so nobody can find out which emails exist.
	ErrBadLogin = errors.New("wrong email or password")
	// ErrNoSession means the session is unknown, ended or expired: log in again.
	ErrNoSession = errors.New("please log in")
)

// Session is a logged-in person, inside one business.
type Session struct {
	PersonID  string
	Name      string
	Roles     []string
	ExpiresAt time.Time
	// MustChange is true while the login's password is a temporary one (an admin made it, before
	// set-your-password links): they choose their own before anything else.
	MustChange bool
	// AccountID and Email are the login used (see account.go).
	AccountID string
	Email     string
}

// ErrWrongPassword means the current password given to change it was wrong.
var ErrWrongPassword = errors.New("the current password is wrong")

// ErrSamePassword means the new password is the current one.
var ErrSamePassword = errors.New("please choose a password different from the current one")

// CheckNew says what is wrong with a new password, or nil if it may be used.
func CheckNew(password string) error {
	switch {
	case utf8.RuneCountInString(password) < MinPassword:
		return fmt.Errorf("a password needs at least %d characters", MinPassword)
	case len(password) > maxPassword:
		return fmt.Errorf("a password can be at most %d bytes", maxPassword)
	}
	return nil
}

func roles(ctx context.Context, tx pgx.Tx, personID string) ([]string, error) {
	rows, _ := tx.Query(ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL
		ORDER BY role_key`, personID)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// newToken is 32 random bytes, written so they fit in a cookie or a web address.
func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
