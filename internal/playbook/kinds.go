// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
)

// Each business has its own lists (master kinds), kept in the database and edited in Settings. The
// files in the master-data folder are only templates to start from.

// Kinds returns the business's lists by kind. tx must come from db.WithTenant.
func Kinds(ctx context.Context, tx pgx.Tx) (map[string]*MasterKind, error) {
	rows, _ := tx.Query(ctx, `SELECT source FROM master_kinds WHERE tenant_id = current_tenant()`)
	sources, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]*MasterKind, len(sources))
	for _, src := range sources {
		m, err := ParseMasterKind([]byte(src))
		if err != nil {
			return nil, err
		}
		out[m.Kind] = m
	}
	return out, nil
}

// Kind returns one of the business's lists, or nil if it has none of that kind.
// tx must come from db.WithTenant.
func Kind(ctx context.Context, tx pgx.Tx, kind string) (*MasterKind, error) {
	var src string
	err := tx.QueryRow(ctx, `SELECT source FROM master_kinds WHERE tenant_id = current_tenant() AND kind = $1`,
		kind).Scan(&src)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseMasterKind([]byte(src))
}

// ForBusiness returns the templates together with the business's own lists, for checking processes.
// tx must come from db.WithTenant.
func ForBusiness(ctx context.Context, tx pgx.Tx, templates *Library) (*Library, error) {
	kinds, err := Kinds(ctx, tx)
	if err != nil {
		return nil, err
	}
	lib := &Library{Masters: kinds}
	if templates != nil {
		lib.Playbooks = templates.Playbooks
	}
	return lib, nil
}

// SaveKind adds a list to the business or changes one. A change is refused if a process the business
// uses needs something the new definition no longer has. Saving the same definition again changes
// nothing (changed = false). tx must come from db.WithTenant.
func SaveKind(ctx context.Context, tx pgx.Tx, m *MasterKind, by ledger.Actor) (changed bool, err error) {
	if err := checkKindUse(ctx, tx, m); err != nil {
		return false, err
	}
	var id string
	var version int
	var stored []byte
	err = tx.QueryRow(ctx, `
		SELECT id::text, version, content_sha256 FROM master_kinds WHERE tenant_id = current_tenant() AND kind = $1`,
		m.Kind).Scan(&id, &version, &stored)
	switch {
	case err == nil:
		if bytes.Equal(stored, m.Hash[:]) {
			return false, nil
		}
		version++
		_, err = tx.Exec(ctx, `
			UPDATE master_kinds SET label = $2, version = $3, content_sha256 = $4, source = $5, updated_at = now()
			WHERE tenant_id = current_tenant() AND kind = $1`, m.Kind, m.Label, version, m.Hash[:], string(m.Source))
	case errors.Is(err, pgx.ErrNoRows):
		version = 1
		err = tx.QueryRow(ctx, `
			INSERT INTO master_kinds (tenant_id, kind, label, content_sha256, source)
			VALUES (current_tenant(), $1, $2, $3, $4) RETURNING id::text`,
			m.Kind, m.Label, m.Hash[:], string(m.Source)).Scan(&id)
	}
	if err != nil {
		return false, fmt.Errorf("playbook: save list %s: %w", m.Kind, err)
	}
	_, err = ledger.Append(ctx, tx, ledger.Event{
		Kind: "master_kind.changed", SubjectType: "master_kind", SubjectID: id,
		Payload: map[string]any{"kind": m.Kind, "label": m.Label, "version": version,
			"sha256": hex.EncodeToString(m.Hash[:])},
	}.By(by))
	return err == nil, err
}

// checkKindUse checks the current version of every process the business has against a new
// definition of one list.
func checkKindUse(ctx context.Context, tx pgx.Tx, m *MasterKind) error {
	inUse, err := ListInUse(ctx, tx)
	if err != nil {
		return err
	}
	lib := &Library{Masters: map[string]*MasterKind{m.Kind: m}}
	for _, u := range inUse {
		if err := u.Playbook.Validate(); err != nil {
			return err
		}
		if err := lib.checkMasterUse(u.Playbook, m.Kind); err != nil {
			return fmt.Errorf("process %s still needs it: %w", u.PlaybookID, err)
		}
	}
	return nil
}

// NeededKinds lists the master kinds a process uses, sorted.
func NeededKinds(p *Playbook) []string {
	set := map[string]bool{}
	for _, f := range p.Fields {
		if f.Type == "master" {
			set[f.Kind] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// EnsureKinds gives the business every list process p needs that it doesn't have yet, copied from
// the templates. Lists it already has are kept as they are. tx must come from db.WithTenant.
func EnsureKinds(ctx context.Context, tx pgx.Tx, templates *Library, p *Playbook, by ledger.Actor) error {
	for _, kind := range NeededKinds(p) {
		has, err := Kind(ctx, tx, kind)
		if err != nil {
			return err
		}
		if has != nil {
			continue
		}
		var t *MasterKind
		if templates != nil {
			t = templates.Masters[kind]
		}
		if t == nil {
			return fmt.Errorf("this process needs a list %q; add it in Settings → Lists first", kind)
		}
		if _, err := SaveKind(ctx, tx, t, by); err != nil {
			return err
		}
	}
	return nil
}

// AdoptKinds gives a business that has processes or master data from before lists were kept per
// business the lists it uses, copied from the templates. It returns the kinds it added, and an error
// naming any it couldn't find. tx must come from db.WithTenant.
func AdoptKinds(ctx context.Context, tx pgx.Tx, templates *Library) (added []string, err error) {
	need := map[string]bool{}
	inUse, err := ListInUse(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, u := range inUse {
		for _, k := range NeededKinds(u.Playbook) {
			need[k] = true
		}
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT kind FROM master_items WHERE tenant_id = current_tenant()`)
	used, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, k := range used {
		need[k] = true
	}
	var missing []string
	for _, kind := range slices.Sorted(maps.Keys(need)) {
		has, err := Kind(ctx, tx, kind)
		if err != nil {
			return added, err
		}
		if has != nil {
			continue
		}
		t := templates.Masters[kind]
		if t == nil {
			missing = append(missing, kind)
			continue
		}
		if _, err := SaveKind(ctx, tx, t, ledger.SystemActor); err != nil {
			return added, err
		}
		added = append(added, kind)
	}
	if len(missing) > 0 {
		return added, fmt.Errorf("no template for the list(s) %v; add them in Settings → Lists", missing)
	}
	return added, nil
}

// MissingDetails returns the fields of a template list that list m doesn't have, in the template's
// order. A nil list or template has none.
func MissingDetails(m, template *MasterKind) []Field {
	if m == nil || template == nil {
		return nil
	}
	var out []Field
	for _, f := range template.Fields {
		if m.field(f.Key) == nil {
			out = append(out, f)
		}
	}
	return out
}

// WithDetails returns list m with extra fields added at the end, written out again as a definition
// (Seyal's own layout, one field per line; comments of the old text are not kept).
func (m *MasterKind) WithDetails(extra []Field) (*MasterKind, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "kind: %s\nlabel: %s\nfields:\n", m.Kind, strconv.Quote(m.Label))
	for _, f := range append(slices.Clone(m.Fields), extra...) {
		fmt.Fprintf(&b, "  - { key: %s, label: %s, type: %s", f.Key, strconv.Quote(f.Label), f.Type)
		if f.Of != "" {
			fmt.Fprintf(&b, ", of: %s", f.Of)
		}
		if len(f.Options) > 0 {
			opts := make([]string, len(f.Options))
			for i, o := range f.Options {
				opts[i] = strconv.Quote(o)
			}
			fmt.Fprintf(&b, ", options: [%s]", strings.Join(opts, ", "))
		}
		if f.Required {
			b.WriteString(", required: true")
		}
		b.WriteString(" }\n")
	}
	return ParseMasterKind([]byte(b.String()))
}
