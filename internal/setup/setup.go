// SPDX-License-Identifier: AGPL-3.0-or-later

// Package setup creates or updates a business from one YAML file: its settings, roles, people, the
// processes and lists it uses, and its master data. Importing the same file again changes nothing,
// so a file can be kept as the business's set-up and re-imported after edits.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
	// The processes the business uses: template ids, or paths (ending in .yaml, relative to this
	// file) to the business's own definitions. Required; [] means none yet.
	Processes []string `yaml:"processes"`
	// Lists (master kinds) the business uses, the same way. Optional: the lists its processes and its
	// master data need are added from the templates anyway.
	Lists []string `yaml:"lists"`
	// Master items by kind. Money is in normal units (12000 = 12,000.00); "@key" is a person.
	Master map[string][]Item `yaml:"master"`

	dir    string            // the file's folder, for relative paths
	chosen *playbook.Library // the processes and lists above, read and checked
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
	f.dir = filepath.Dir(path)
	if err := f.Check(lib); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Check reports every problem in the file at once. lib holds the templates.
func (f *File) Check(lib *playbook.Library) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	problems = append(problems, f.choose(lib)...)

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
		mk := f.chosen.Masters[kind]
		if mk == nil {
			continue // reported by choose
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

// choose reads the processes and lists the file names, from the templates or the business's own
// files, adds the lists they need, and checks that they fit together. It returns the problems.
func (f *File) choose(templates *playbook.Library) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	f.chosen = &playbook.Library{Playbooks: map[string]*playbook.Playbook{}, Masters: map[string]*playbook.MasterKind{}}
	own := func(entry string) bool { return strings.HasSuffix(entry, ".yaml") }

	for _, entry := range f.Lists {
		mk := templates.Masters[entry]
		if own(entry) {
			var err error
			if mk, err = playbook.LoadMasterKindFile(filepath.Join(f.dir, entry)); err != nil {
				add("list %s: %v", entry, err)
				continue
			}
		}
		if mk == nil {
			add("list %q: there is no such template (or give the path to your own .yaml file)", entry)
			continue
		}
		f.chosen.Masters[mk.Kind] = mk
	}
	if f.Processes == nil {
		add("processes: list the processes this business uses (template ids or .yaml files), or [] for none yet")
	}
	for _, entry := range f.Processes {
		p := templates.Playbooks[entry]
		if own(entry) {
			var err error
			if p, err = playbook.LoadFile(filepath.Join(f.dir, entry)); err != nil {
				add("process %s: %v", entry, err)
				continue
			}
		}
		if p == nil {
			add("process %q: there is no such template (or give the path to your own .yaml file)", entry)
			continue
		}
		f.chosen.Playbooks[p.ID] = p
	}

	// Processes that the chosen ones start as linked work come from the templates too.
	for again := true; again; {
		again = false
		for _, p := range slices.Collect(maps.Values(f.chosen.Playbooks)) {
			for _, id := range playbook.StartsProcesses(p) {
				if f.chosen.Playbooks[id] != nil {
					continue
				}
				if t := templates.Playbooks[id]; t != nil {
					f.chosen.Playbooks[id], again = t, true
				} else {
					add("process %s starts %q, which isn't listed and has no template", p.ID, id)
				}
			}
		}
	}

	// Lists that the processes or the master data need, and that weren't named, come from the templates.
	need := slices.Collect(maps.Keys(f.Master))
	for _, p := range f.chosen.Playbooks {
		need = append(need, playbook.NeededKinds(p)...)
	}
	slices.Sort(need)
	for _, kind := range slices.Compact(need) {
		if f.chosen.Masters[kind] != nil {
			continue
		}
		if mk := templates.Masters[kind]; mk != nil {
			f.chosen.Masters[kind] = mk
		} else {
			add("list %q is used but not defined; add it under lists", kind)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(f.chosen.Playbooks)) {
		if err := f.chosen.Check(f.chosen.Playbooks[id]); err != nil {
			add("process %s: %v", id, err)
		}
	}
	return problems
}

// Import creates or updates the business described by f. The owner pool creates the business
// (the app role may not); everything else goes through the app pool, with ledger events.
// Lists and processes the business already has keep their current definitions (those are changed in
// Settings); new ones are added.
func Import(ctx context.Context, owner, app *pgxpool.Pool, f *File) (Summary, error) {
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
		kinds, err := playbook.Kinds(ctx, tx)
		if err != nil {
			return err
		}
		for _, kind := range slices.Sorted(maps.Keys(f.chosen.Masters)) {
			if kinds[kind] == nil {
				if err := count(playbook.SaveKind(ctx, tx, f.chosen.Masters[kind], by)); err != nil {
					return err
				}
				kinds[kind] = f.chosen.Masters[kind]
			}
		}
		// A process the business already has keeps the version it uses (changed in Settings).
		for _, id := range slices.Sorted(maps.Keys(f.chosen.Playbooks)) {
			_, isNew, err := playbook.Register(ctx, tx, f.chosen.Playbooks[id])
			if err == nil {
				_, err = playbook.EnsureUsed(ctx, tx, f.chosen.Playbooks[id], by)
			}
			if err := count(isNew, err); err != nil {
				return err
			}
		}
		for _, kind := range slices.Sorted(maps.Keys(f.Master)) {
			mk := kinds[kind] // the business's own definition, which may differ from the file's
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

// NewPassword gives the login with this email a new random password (auth.NewPassword), making the
// login if there is none, and returns the password: shown once and never stored. The password
// changes in every business the login already belongs to. With a tenantID, the login is also linked
// to that business's person with this email (an error if there is none, or if that person already
// has another login). Everything happens in one transaction: on an error nothing changes.
func NewPassword(ctx context.Context, app *pgxpool.Pool, email, tenantID string) (string, error) {
	var password string
	var err error
	if tenantID == "" {
		err = db.WithServer(ctx, app, func(tx pgx.Tx) error {
			var err error
			password, _, err = auth.NewPassword(ctx, tx, email, "")
			return err
		})
		return password, err
	}
	err = db.WithTenant(ctx, app, tenantID, func(tx pgx.Tx) error {
		personID, err := people.ByEmail(ctx, tx, email)
		if err != nil {
			return err
		}
		password, _, err = auth.NewPassword(ctx, tx, email, personID)
		return err
	})
	return password, err
}

// Link adds an existing login to a business (`seyal link`): the login with this email now reaches
// the business's person with the same email. The password doesn't change and no session ends, so
// nothing changes for the login's other businesses. It returns false if they were already linked.
// The operator is trusted, like set-password; a business's admin can do the same only with a link
// that asks for the login's password (D64).
func Link(ctx context.Context, app *pgxpool.Pool, email, tenantID string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var linked bool
	err := db.WithTenant(ctx, app, tenantID, func(tx pgx.Tx) error {
		accountID, err := auth.FindAccount(ctx, tx, email)
		if err != nil {
			return err
		}
		if accountID == "" {
			return fmt.Errorf("there is no login with the email %s yet; to make one, use seyal set-password", email)
		}
		personID, err := people.ByEmail(ctx, tx, email)
		if err != nil {
			return fmt.Errorf("%w in this business; add them first (Settings → People, or the set-up file)", err)
		}
		var current string
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(account_id::text, '') FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			personID).Scan(&current); err != nil {
			return err
		}
		if current == accountID {
			return nil
		}
		linked = true
		return auth.Link(ctx, tx, accountID, personID, "operator", ledger.SystemActor)
	})
	return linked, err
}
