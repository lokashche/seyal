// SPDX-License-Identifier: AGPL-3.0-or-later

// Package people manages the people of a business and their roles. Every change is written to the
// ledger in the same transaction.
package people

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// DefineRole adds a role, or renames it. It returns whether anything changed.
// tx must come from db.WithTenant.
func DefineRole(ctx context.Context, tx pgx.Tx, key, name string, by ledger.Actor) (bool, error) {
	var old string
	err := tx.QueryRow(ctx, `SELECT name FROM roles WHERE tenant_id = current_tenant() AND key = $1`, key).Scan(&old)
	switch {
	case err == nil && old == name:
		return false, nil
	case err == nil:
		_, err = tx.Exec(ctx, `UPDATE roles SET name = $2 WHERE tenant_id = current_tenant() AND key = $1`, key, name)
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `INSERT INTO roles (tenant_id, key, name) VALUES (current_tenant(), $1, $2)`, key, name)
	}
	if err != nil {
		return false, fmt.Errorf("people: role %s: %w", key, err)
	}
	return true, record(ctx, tx, "role.defined", "role", "", map[string]string{"key": key, "name": name}, by)
}

// Ensure finds the person with this login email, or adds them, and keeps their name up to date.
// It returns the person's id and whether anything changed. tx must come from db.WithTenant.
func Ensure(ctx context.Context, tx pgx.Tx, name, email string, by ledger.Actor) (id string, changed bool, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var old string
	err = tx.QueryRow(ctx, `
		SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() AND login_email = $1`,
		email).Scan(&id, &old)
	switch {
	case err == nil && old == name:
		return id, false, nil
	case err == nil:
		if _, err = tx.Exec(ctx, `UPDATE persons SET display_name = $2 WHERE tenant_id = current_tenant() AND id::text = $1`,
			id, name); err != nil {
			return "", false, fmt.Errorf("people: rename: %w", err)
		}
		return id, true, record(ctx, tx, "person.renamed", "person", id, map[string]string{"name": name}, by)
	case !errors.Is(err, pgx.ErrNoRows):
		return "", false, err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO persons (tenant_id, display_name, login_email) VALUES (current_tenant(), $1, $2)
		RETURNING id::text`, name, email).Scan(&id); err != nil {
		return "", false, fmt.Errorf("people: add %s: %w", email, err)
	}
	return id, true, record(ctx, tx, "person.created", "person", id, map[string]string{"name": name, "email": email}, by)
}

// Grant gives a person a role, if they don't have it yet. It returns whether anything changed.
// tx must come from db.WithTenant.
func Grant(ctx context.Context, tx pgx.Tx, personID, role string, by ledger.Actor) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES (current_tenant(), $1, $2)
		ON CONFLICT DO NOTHING`, personID, role)
	if err != nil {
		return false, fmt.Errorf("people: grant %s: %w", role, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, record(ctx, tx, "role.granted", "person", personID, map[string]string{"role": role}, by)
}

// ByEmail returns the id of the person with this login email. tx must come from db.WithTenant.
func ByEmail(ctx context.Context, tx pgx.Tx, email string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM persons WHERE tenant_id = current_tenant() AND login_email = $1`,
		strings.ToLower(strings.TrimSpace(email))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("nobody has the login email %s", email)
	}
	return id, err
}

func record(ctx context.Context, tx pgx.Tx, kind, subjectType, subjectID string, payload any, by ledger.Actor) error {
	_, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: kind, SubjectType: subjectType, SubjectID: subjectID, Payload: payload,
	}.By(by))
	return err
}
