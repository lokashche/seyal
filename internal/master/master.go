// SPDX-License-Identifier: AGPL-3.0-or-later

// Package master stores master-data items (the entries of each master kind) and lets the engine
// look them up. Every change is written to the ledger in the same transaction.
package master

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Item is one master-data entry.
type Item struct {
	ID      string
	Kind    string
	Key     string // short id used in work items, e.g. "monthly-books"
	Name    string // what people see
	Fields  engine.Fields
	Version int
	Active  bool
}

// ErrNotFound means there is no item with that kind and key.
var ErrNotFound = errors.New("master item not found")

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Save adds an item or updates it. It checks the fields against the kind, bumps the version, and
// records master.changed in the ledger. Saving an inactive item makes it active again. Saving
// exactly what is already stored changes nothing and returns changed = false.
// tx must come from db.WithTenant.
func Save(ctx context.Context, tx pgx.Tx, kind *playbook.MasterKind, key, name string,
	fields engine.Fields, by ledger.Actor) (it Item, changed bool, err error) {
	fields, err = check(kind, key, name, fields)
	if err != nil {
		return Item{}, false, err
	}
	old, err := Get(ctx, tx, kind.Kind, key)
	switch {
	case err == nil:
		if old.Active && old.Name == name && same(old.Fields, fields) {
			return old, false, nil
		}
		err = tx.QueryRow(ctx, `
			UPDATE master_items SET name = $3, fields = $4, version = version + 1, active = true, updated_at = now()
			WHERE tenant_id = current_tenant() AND kind = $1 AND key = $2
			RETURNING id::text, version`, kind.Kind, key, name, fields).Scan(&it.ID, &it.Version)
	case errors.Is(err, ErrNotFound):
		err = tx.QueryRow(ctx, `
			INSERT INTO master_items (tenant_id, kind, key, name, fields)
			VALUES (current_tenant(), $1, $2, $3, $4)
			RETURNING id::text, version`, kind.Kind, key, name, fields).Scan(&it.ID, &it.Version)
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("master: save %s %s: %w", kind.Kind, key, err)
	}
	it.Kind, it.Key, it.Name, it.Fields, it.Active = kind.Kind, key, name, fields, true

	if err := record(ctx, tx, "master.changed", it, by); err != nil {
		return Item{}, false, err
	}
	return it, true, nil
}

// Deactivate hides an item from new choices. Work items that already use it keep it.
// tx must come from db.WithTenant.
func Deactivate(ctx context.Context, tx pgx.Tx, kind, key string, by ledger.Actor) error {
	it, err := Get(ctx, tx, kind, key)
	if err != nil {
		return err
	}
	if !it.Active {
		return nil
	}
	err = tx.QueryRow(ctx, `
		UPDATE master_items SET active = false, version = version + 1, updated_at = now()
		WHERE tenant_id = current_tenant() AND kind = $1 AND key = $2
		RETURNING version`, kind, key).Scan(&it.Version)
	if err != nil {
		return fmt.Errorf("master: deactivate %s %s: %w", kind, key, err)
	}
	it.Active = false
	return record(ctx, tx, "master.deactivated", it, by)
}

// Get returns one item, active or not. It returns ErrNotFound if there is none.
// tx must come from db.WithTenant.
func Get(ctx context.Context, tx pgx.Tx, kind, key string) (Item, error) {
	it := Item{Kind: kind, Key: key}
	err := tx.QueryRow(ctx, `
		SELECT id::text, name, fields, version, active FROM master_items
		WHERE tenant_id = current_tenant() AND kind = $1 AND key = $2`, kind, key).
		Scan(&it.ID, &it.Name, &it.Fields, &it.Version, &it.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, fmt.Errorf("%s %q: %w", kind, key, ErrNotFound)
	}
	if err != nil {
		return Item{}, fmt.Errorf("master: get %s %s: %w", kind, key, err)
	}
	return it, nil
}

var nonKey = regexp.MustCompile(`[^a-z0-9]+`)

// FreeKey makes a key from a name ("Acme & Sons" → "acme-sons"), adding "-2", "-3", … if the key is
// taken. tx must come from db.WithTenant.
func FreeKey(ctx context.Context, tx pgx.Tx, kind, name string) (string, error) {
	base := strings.Trim(nonKey.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		return "", errors.New("please give it a name")
	}
	for i := 1; ; i++ {
		key := base
		if i > 1 {
			key = fmt.Sprintf("%s-%d", base, i)
		}
		_, err := Get(ctx, tx, kind, key)
		if errors.Is(err, ErrNotFound) {
			return key, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// LookupFor loads the master items a work item's fields point at, and returns a lookup the
// engine can use. Items that don't exist are simply missing from the lookup, so rules that need
// them answer "unknown". tx must come from db.WithTenant.
func LookupFor(ctx context.Context, tx pgx.Tx, p *playbook.Playbook, fields engine.Fields) (engine.MasterLookup, error) {
	found := map[string]engine.Fields{} // "kind/key" → fields
	for _, f := range p.Fields {
		key, _ := fields[f.Key].(string)
		if f.Type != "master" || key == "" {
			continue
		}
		it, err := Get(ctx, tx, f.Kind, key)
		switch {
		case err == nil:
			found[f.Kind+"/"+key] = it.Fields
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}
	}
	return func(kind, key string) (engine.Fields, bool) {
		f, ok := found[kind+"/"+key]
		return f, ok
	}, nil
}

// check validates an item against its kind and returns its fields without empty values.
func check(kind *playbook.MasterKind, key, name string, fields engine.Fields) (engine.Fields, error) {
	var problems []string
	if !keyPattern.MatchString(key) {
		problems = append(problems, fmt.Sprintf("key %q must use lowercase letters, digits, - and _", key))
	}
	if strings.TrimSpace(name) == "" {
		problems = append(problems, "name is required")
	}
	clean := engine.Fields{}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		v := fields[k]
		i := slices.IndexFunc(kind.Fields, func(f playbook.Field) bool { return f.Key == k })
		switch {
		case i < 0:
			problems = append(problems, fmt.Sprintf("%s has no field %q", kind.Kind, k))
		case v == nil:
			// an empty value just isn't stored
		default:
			if err := engine.CheckValue(kind.Fields[i], v); err != nil {
				problems = append(problems, err.Error())
				continue
			}
			clean[k] = v
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s %q: %s", kind.Kind, key, strings.Join(problems, "; "))
	}
	return clean, nil
}

// same reports whether two sets of fields are identical.
func same(a, b engine.Fields) bool {
	ha, errA := engine.HashFields(a)
	hb, errB := engine.HashFields(b)
	return errA == nil && errB == nil && ha == hb
}

func record(ctx context.Context, tx pgx.Tx, kind string, it Item, by ledger.Actor) error {
	_, err := ledger.Append(ctx, tx, ledger.Event{
		Kind:        kind,
		SubjectType: "master_item",
		SubjectID:   it.ID,
		Payload: map[string]any{
			"kind": it.Kind, "key": it.Key, "name": it.Name, "fields": it.Fields,
			"version": it.Version, "active": it.Active,
		},
	}.By(by))
	return err
}
