// SPDX-License-Identifier: AGPL-3.0-or-later

// Package setup creates or updates a business from one YAML file: its settings, roles, people,
// master data, and the playbooks it uses. Importing the same file again changes nothing, so a file
// can be kept as the business's set-up and re-imported after edits.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/people"
	"seyal/internal/playbook"
)

// File is a business's set-up file.
type File struct {
	Business Business `yaml:"business"`
	Roles    []Role   `yaml:"roles"`
	People   []Person `yaml:"people"`
	// Master items by kind. Money is in normal units (12000 = 12,000.00); "@key" is a person.
	Master map[string][]Item `yaml:"master"`
}

type Business struct {
	Slug                 string `yaml:"slug"` // the business's part of the web address, /t/<slug>/
	Name                 string `yaml:"name"`
	Currency             string `yaml:"currency"`
	Timezone             string `yaml:"timezone"`
	FiscalYearStartMonth int    `yaml:"fiscal_year_start_month"`
}

type Role struct {
	Key  string `yaml:"key"`
	Name string `yaml:"name"`
}

type Person struct {
	Key   string   `yaml:"key"` // used for "@key" references in this file only
	Name  string   `yaml:"name"`
	Email string   `yaml:"email"`
	Roles []string `yaml:"roles"`
}

type Item struct {
	Key    string         `yaml:"key"`
	Name   string         `yaml:"name"`
	Fields map[string]any `yaml:"fields"`
}

// Summary says what an import changed.
type Summary struct {
	TenantID string
	Created  bool              // the business is new
	Changes  int               // roles, people, grants, master items and playbooks added or changed
	People   map[string]string // file key → person id
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// Load reads and checks a set-up file against the library of playbooks and master kinds.
func Load(path string, lib *playbook.Library) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := f.Check(lib); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Check reports every problem in the file at once.
func (f *File) Check(lib *playbook.Library) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	b := f.Business
	if !slugPattern.MatchString(b.Slug) {
		add("business slug %q: use lowercase letters, digits and -", b.Slug)
	}
	if strings.TrimSpace(b.Name) == "" {
		add("business name is required")
	}
	if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(b.Currency) {
		add("currency %q: use a 3-letter code such as INR or USD", b.Currency)
	}
	if _, err := time.LoadLocation(b.Timezone); err != nil || b.Timezone == "" {
		add("time zone %q is unknown (for example Asia/Kolkata or UTC)", b.Timezone)
	}
	if b.FiscalYearStartMonth < 1 || b.FiscalYearStartMonth > 12 {
		add("fiscal_year_start_month must be 1 to 12")
	}

	roles := map[string]bool{}
	for _, r := range f.Roles {
		if roles[r.Key] {
			add("role %q is listed twice", r.Key)
		}
		roles[r.Key] = true
	}
	keys, emails := map[string]bool{}, map[string]bool{}
	for _, p := range f.People {
		email := strings.ToLower(p.Email)
		switch {
		case p.Key == "" || keys[p.Key]:
			add("person %q: each person needs a unique key", p.Name)
		case strings.TrimSpace(p.Name) == "":
			add("person %s: name is required", p.Key)
		case !strings.Contains(email, "@") || emails[email]:
			add("person %s: needs its own email address", p.Key)
		}
		keys[p.Key], emails[email] = true, true
		for _, r := range p.Roles {
			if !roles[r] {
				add("person %s: role %q is not listed under roles", p.Key, r)
			}
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(f.Master)) {
		mk := lib.Masters[kind]
		if mk == nil {
			add("master kind %q is not defined in the master-data files", kind)
			continue
		}
		for _, it := range f.Master[kind] {
			if _, err := f.fields(mk, it, map[string]string{}, keys); err != nil {
				add("%s %s: %v", kind, it.Key, err)
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New("set-up file has problems:\n  - " + strings.Join(problems, "\n  - "))
}

// Import creates or updates the business described by f. The owner pool creates the business
// (the app role may not); everything else goes through the app pool, with ledger events.
func Import(ctx context.Context, owner, app *pgxpool.Pool, lib *playbook.Library, f *File) (Summary, error) {
	sum := Summary{People: map[string]string{}}
	by := ledger.SystemActor
	b := f.Business

	err := owner.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, b.Slug).Scan(&sum.TenantID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		sum.Created = true
		err = owner.QueryRow(ctx, `
			INSERT INTO tenants (slug, name, currency, timezone, fiscal_year_start_month)
			VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
			b.Slug, b.Name, b.Currency, b.Timezone, b.FiscalYearStartMonth).Scan(&sum.TenantID)
	case err == nil:
		tag, updateErr := owner.Exec(ctx, `
			UPDATE tenants SET name = $2, currency = $3, timezone = $4, fiscal_year_start_month = $5
			WHERE id::text = $1
				AND (name, currency, timezone, fiscal_year_start_month) IS DISTINCT FROM ($2, $3, $4, $5)`,
			sum.TenantID, b.Name, b.Currency, b.Timezone, b.FiscalYearStartMonth)
		if updateErr == nil && tag.RowsAffected() > 0 {
			sum.Changes++
		}
		err = updateErr
	}
	if err != nil {
		return sum, fmt.Errorf("setup: business: %w", err)
	}

	err = db.WithTenant(ctx, app, sum.TenantID, func(tx pgx.Tx) error {
		count := func(changed bool, err error) error {
			if changed {
				sum.Changes++
			}
			return err
		}
		if sum.Created {
			if _, err := ledger.Append(ctx, tx, ledger.Event{Kind: "business.created", SubjectType: "business",
				SubjectID: sum.TenantID, Payload: b}.By(by)); err != nil {
				return err
			}
		}
		for _, r := range f.Roles {
			if err := count(people.DefineRole(ctx, tx, r.Key, r.Name, by)); err != nil {
				return err
			}
		}
		for _, p := range f.People {
			id, changed, err := people.Ensure(ctx, tx, p.Name, p.Email, by)
			if err := count(changed, err); err != nil {
				return err
			}
			sum.People[p.Key] = id
			for _, r := range p.Roles {
				if err := count(people.Grant(ctx, tx, id, r, by)); err != nil {
					return err
				}
			}
		}
		// The business starts using each process from the folder; one it already has keeps the
		// version it uses (changing that is done in Settings → Processes).
		for _, id := range slices.Sorted(maps.Keys(lib.Playbooks)) {
			_, isNew, err := playbook.Register(ctx, tx, lib.Playbooks[id])
			if err == nil {
				_, err = playbook.EnsureUsed(ctx, tx, lib.Playbooks[id], by)
			}
			if err := count(isNew, err); err != nil {
				return err
			}
		}
		for _, kind := range slices.Sorted(maps.Keys(f.Master)) {
			mk := lib.Masters[kind]
			for _, it := range f.Master[kind] {
				fields, err := f.fields(mk, it, sum.People, nil)
				if err != nil {
					return fmt.Errorf("%s %s: %w", kind, it.Key, err)
				}
				_, changed, err := master.Save(ctx, tx, mk, it.Key, it.Name, fields, by)
				if err := count(changed, err); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return sum, err
}

// fields converts an item's values from the file's friendly form into stored values: money from
// normal units to minor units, "@key" to person ids, YAML numbers to float64. ids maps person keys
// to ids; when checking a file before import, known holds the person keys instead.
func (f *File) fields(mk *playbook.MasterKind, it Item, ids map[string]string, known map[string]bool) (engine.Fields, error) {
	out := engine.Fields{}
	person := func(v any) (any, error) {
		s, _ := v.(string)
		key, ok := strings.CutPrefix(s, "@")
		switch {
		case !ok:
			return nil, fmt.Errorf("a person must be written as @key, not %v", v)
		case known != nil && known[key]:
			return "00000000-0000-0000-0000-000000000000", nil // placeholder while checking
		case ids[key] != "":
			return ids[key], nil
		}
		return nil, fmt.Errorf("unknown person @%s", key)
	}
	for _, key := range slices.Sorted(maps.Keys(it.Fields)) {
		v := it.Fields[key]
		i := slices.IndexFunc(mk.Fields, func(fd playbook.Field) bool { return fd.Key == key })
		if i < 0 {
			return nil, fmt.Errorf("%s has no field %q", mk.Kind, key)
		}
		fd := mk.Fields[i]
		var err error
		switch {
		case fd.Type == "person":
			v, err = person(v)
		case fd.Type == "list" && fd.Of == "person":
			items, _ := v.([]any)
			list := make([]any, len(items))
			for j, x := range items {
				if list[j], err = person(x); err != nil {
					break
				}
			}
			v = list
		case fd.Type == "money":
			if x, ok := toFloat(v); ok {
				v = x * 100
			}
		case fd.Type == "number":
			if x, ok := toFloat(v); ok {
				v = x
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if err := engine.CheckValue(fd, v); err != nil {
			return nil, err
		}
		out[key] = v
	}
	if strings.TrimSpace(it.Name) == "" {
		return nil, errors.New("name is required")
	}
	return out, nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// NewPassword gives the person with this email a new random password (auth.NewPassword) and returns
// it. It is shown once and never stored.
func NewPassword(ctx context.Context, app *pgxpool.Pool, tenantID, email string) (string, error) {
	var password string
	err := db.WithTenant(ctx, app, tenantID, func(tx pgx.Tx) error {
		id, err := people.ByEmail(ctx, tx, email)
		if err != nil {
			return err
		}
		password, err = auth.NewPassword(ctx, tx, id, ledger.SystemActor)
		return err
	})
	return password, err
}
