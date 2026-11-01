// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// A business chooses which processes (playbooks) it uses and which version new work starts on.
// Work already started always keeps the version it started with.

// InUse is a process a business has, with its current version and every version it has had.
type InUse struct {
	PlaybookID string
	Active     bool
	Current    VersionInfo
	Playbook   *Playbook // the current version, parsed
	Versions   []VersionInfo
}

// VersionInfo describes one registered version.
type VersionInfo struct {
	ID, Version  string
	RegisteredAt time.Time
	Source       string
}

// Use makes versionID the version new work starts on, and switches the process on.
// tx must come from db.WithTenant.
func Use(ctx context.Context, tx pgx.Tx, versionID string, by ledger.Actor) error {
	var playbookID, version string
	err := tx.QueryRow(ctx, `
		SELECT playbook_id, version FROM playbook_versions WHERE tenant_id = current_tenant() AND id::text = $1`,
		versionID).Scan(&playbookID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("that version doesn't exist")
	}
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO tenant_playbooks (tenant_id, playbook_id, current_version_id)
		VALUES (current_tenant(), $1, $2::uuid)
		ON CONFLICT (tenant_id, playbook_id) DO UPDATE
			SET current_version_id = EXCLUDED.current_version_id, active = true, updated_at = now()
			WHERE tenant_playbooks.current_version_id <> EXCLUDED.current_version_id OR NOT tenant_playbooks.active`,
		playbookID, versionID)
	if err != nil {
		return fmt.Errorf("playbook: use: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "playbook.used", SubjectType: "playbook_version", SubjectID: versionID,
		Payload: map[string]string{"playbook": playbookID, "version": version},
	}.By(by))
	return err
}

// SetActive switches a process on or off for new work. Work already started carries on.
// tx must come from db.WithTenant.
func SetActive(ctx context.Context, tx pgx.Tx, playbookID string, active bool, by ledger.Actor) error {
	tag, err := tx.Exec(ctx, `
		UPDATE tenant_playbooks SET active = $2, updated_at = now()
		WHERE tenant_id = current_tenant() AND playbook_id = $1 AND active <> $2`, playbookID, active)
	if err != nil {
		return fmt.Errorf("playbook: switch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	kind := "playbook.disabled"
	if active {
		kind = "playbook.enabled"
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: kind, SubjectType: "playbook", Payload: map[string]string{"playbook": playbookID},
	}.By(by))
	return err
}

// EnsureUsed registers p and, if the business doesn't have this process yet, starts using it. A
// business that already has it keeps its current version. tx must come from db.WithTenant.
func EnsureUsed(ctx context.Context, tx pgx.Tx, p *Playbook, by ledger.Actor) (versionID string, err error) {
	versionID, _, err = Register(ctx, tx, p)
	if err != nil {
		return "", err
	}
	var has bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM tenant_playbooks WHERE tenant_id = current_tenant() AND playbook_id = $1)`,
		p.ID).Scan(&has); err != nil {
		return "", err
	}
	if has {
		return versionID, nil
	}
	return versionID, Use(ctx, tx, versionID, by)
}

// ListInUse returns the business's processes, active ones first. tx must come from db.WithTenant.
func ListInUse(ctx context.Context, tx pgx.Tx) ([]InUse, error) {
	rows, _ := tx.Query(ctx, `
		SELECT playbook_id, active, current_version_id::text FROM tenant_playbooks
		WHERE tenant_id = current_tenant() ORDER BY active DESC, playbook_id`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID      string
		Active  bool
		Current string
	}])
	if err != nil {
		return nil, err
	}
	out := make([]InUse, 0, len(list))
	for _, l := range list {
		u := InUse{PlaybookID: l.ID, Active: l.Active}
		rows, _ := tx.Query(ctx, `
			SELECT id::text, version, registered_at, source FROM playbook_versions
			WHERE tenant_id = current_tenant() AND playbook_id = $1 ORDER BY registered_at DESC`, l.ID)
		if u.Versions, err = pgx.CollectRows(rows, pgx.RowToStructByPos[VersionInfo]); err != nil {
			return nil, err
		}
		for _, v := range u.Versions {
			if v.ID == l.Current {
				u.Current = v
			}
		}
		if u.Playbook, err = Parse([]byte(u.Current.Source)); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}
