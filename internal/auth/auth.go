// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth handles logins: passwords (stored only as bcrypt hashes) and sessions (random tokens
// whose SHA-256 is stored). One login per person.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"seyal/internal/ledger"
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

// Session is a logged-in person.
type Session struct {
	PersonID  string
	Name      string
	Roles     []string
	ExpiresAt time.Time
}

// dummyHash is compared against when an email is unknown, so a wrong email takes as long as a
// wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not a real password"), bcrypt.DefaultCost)

// SetPassword sets a person's login email and password. The ledger records that it happened, never
// the password or its hash. tx must come from db.WithTenant.
func SetPassword(ctx context.Context, tx pgx.Tx, personID, email, password string, by ledger.Actor) error {
	email = strings.ToLower(strings.TrimSpace(email))
	switch {
	case !strings.Contains(email, "@"):
		return errors.New("that doesn't look like an email address")
	case utf8.RuneCountInString(password) < MinPassword:
		return fmt.Errorf("a password needs at least %d characters", MinPassword)
	case len(password) > maxPassword:
		return fmt.Errorf("a password can be at most %d bytes", maxPassword)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE persons SET login_email = $2, password_hash = $3
		WHERE tenant_id = current_tenant() AND id::text = $1`, personID, email, string(hash))
	if err != nil {
		return fmt.Errorf("auth: set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("auth: no such person")
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.login_set", SubjectType: "person", SubjectID: personID,
		Payload: map[string]string{"email": email},
	}.By(by))
	return err
}

// Login checks an email and password and starts a session. It returns the token to put in the
// browser's cookie. tx must come from db.WithTenant (the tenant comes from the address).
func Login(ctx context.Context, tx pgx.Tx, email, password, userAgent string) (token string, s Session, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var hash string
	err = tx.QueryRow(ctx, `
		SELECT id::text, display_name, coalesce(password_hash, '') FROM persons
		WHERE tenant_id = current_tenant() AND login_email = $1 AND status = 'active'`, email).
		Scan(&s.PersonID, &s.Name, &hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && hash == ""):
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password)) // same time as a wrong password
		return "", Session{}, ErrBadLogin
	case err != nil:
		return "", Session{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", Session{}, ErrBadLogin
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	s.ExpiresAt = time.Now().Add(SessionLength).UTC().Truncate(time.Second)
	if len(userAgent) > 500 {
		userAgent = userAgent[:500]
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (token_sha256, tenant_id, person_id, expires_at, user_agent)
		VALUES ($1, current_tenant(), $2, $3, $4)`,
		fingerprint(token), s.PersonID, s.ExpiresAt, strings.ToValidUTF8(userAgent, "")); err != nil {
		return "", Session{}, fmt.Errorf("auth: start session: %w", err)
	}
	if _, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.logged_in", SubjectType: "person", SubjectID: s.PersonID,
	}.By(ledger.PersonActor(s.PersonID))); err != nil {
		return "", Session{}, err
	}
	s.Roles, err = roles(ctx, tx, s.PersonID)
	return token, s, err
}

// NewPassword gives a person a new random password and returns it. It is shown once and never
// stored. Passwords are 16 characters from A-Z and 2-7, so there's no 0/O or 1/l to mix up when
// reading one out. The person must already have a login email. tx must come from db.WithTenant.
func NewPassword(ctx context.Context, tx pgx.Tx, personID string, by ledger.Actor) (string, error) {
	var email string
	err := tx.QueryRow(ctx, `
		SELECT coalesce(login_email, '') FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
		personID).Scan(&email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", errors.New("auth: no such person")
	case err != nil:
		return "", err
	case email == "":
		return "", errors.New("this person has no login email yet")
	}
	password := rand.Text()[:16]
	return password, SetPassword(ctx, tx, personID, email, password, by)
}

// Pretend spends as long as checking a real password. Use it for a login to an address that
// doesn't exist, so the answer can't reveal which addresses do.
func Pretend(password string) {
	bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}

// Check returns the session for a token from a cookie, or ErrNoSession. A person who was disabled
// is logged out at once. tx must come from db.WithTenant.
func Check(ctx context.Context, tx pgx.Tx, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	var s Session
	err := tx.QueryRow(ctx, `
		SELECT s.person_id::text, p.display_name, s.expires_at
		FROM sessions s JOIN persons p ON p.tenant_id = s.tenant_id AND p.id = s.person_id
		WHERE s.tenant_id = current_tenant() AND s.token_sha256 = $1
			AND s.ended_at IS NULL AND s.expires_at > now() AND p.status = 'active'`,
		fingerprint(token)).Scan(&s.PersonID, &s.Name, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	s.Roles, err = roles(ctx, tx, s.PersonID)
	return s, err
}

// Logout ends the session. tx must come from db.WithTenant.
func Logout(ctx context.Context, tx pgx.Tx, token string) error {
	var personID string
	err := tx.QueryRow(ctx, `
		UPDATE sessions SET ended_at = now()
		WHERE tenant_id = current_tenant() AND token_sha256 = $1 AND ended_at IS NULL
		RETURNING person_id::text`, fingerprint(token)).Scan(&personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already logged out
	}
	if err != nil {
		return err
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.logged_out", SubjectType: "person", SubjectID: personID,
	}.By(ledger.PersonActor(personID)))
	return err
}

func roles(ctx context.Context, tx pgx.Tx, personID string) ([]string, error) {
	rows, _ := tx.Query(ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL
		ORDER BY role_key`, personID)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
