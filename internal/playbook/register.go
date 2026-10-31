// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// Register records this playbook version for the current tenant and returns its id. It is safe
// to call on every start: a version that is already registered with the same content is simply
// returned (isNew is false). The same id and version with different content is refused, because
// work items started under the old content must keep pointing at exactly that content.
// tx must come from db.WithTenant.
func Register(ctx context.Context, tx pgx.Tx, p *Playbook) (id string, isNew bool, err error) {
	var storedHash []byte
	err = tx.QueryRow(ctx, `
		SELECT id::text, content_sha256 FROM playbook_versions
		WHERE tenant_id = current_tenant() AND playbook_id = $1 AND version = $2`,
		p.ID, p.Version).Scan(&id, &storedHash)
	switch {
	case err == nil:
		if !bytes.Equal(storedHash, p.Hash[:]) {
			return "", false, fmt.Errorf(
				"playbook %q version %q was changed after it was first used; give it a new version", p.ID, p.Version)
		}
		return id, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", false, err
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO playbook_versions (tenant_id, playbook_id, version, content_sha256, source)
		VALUES (current_tenant(), $1, $2, $3, $4) RETURNING id::text`,
		p.ID, p.Version, p.Hash[:], string(p.Source)).Scan(&id)
	if err != nil {
		return "", false, err
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind:        "playbook.registered",
		SubjectType: "playbook_version",
		SubjectID:   id,
		ActorKind:   ledger.System,
		Payload:     map[string]string{"playbook": p.ID, "version": p.Version, "sha256": hex.EncodeToString(p.Hash[:])},
	})
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}
