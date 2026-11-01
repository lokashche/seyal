// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Library holds every playbook and master kind, loaded and checked together.
type Library struct {
	Playbooks map[string]*Playbook   // by id
	Masters   map[string]*MasterKind // by kind
}

// LoadLibrary reads every .yaml file in the two folders, validates each one, and checks that
// playbooks only use master kinds and master fields that exist. It reports all problems at once.
func LoadLibrary(playbooksDir, mastersDir string) (*Library, error) {
	lib := &Library{Playbooks: map[string]*Playbook{}, Masters: map[string]*MasterKind{}}
	var errs []error

	masterFiles, err := yamlFiles(mastersDir)
	if err != nil {
		return nil, err
	}
	for _, path := range masterFiles {
		m, err := LoadMasterKindFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if lib.Masters[m.Kind] != nil {
			errs = append(errs, fmt.Errorf("%s: master kind %q is defined twice", path, m.Kind))
			continue
		}
		lib.Masters[m.Kind] = m
	}

	playbookFiles, err := yamlFiles(playbooksDir)
	if err != nil {
		return nil, err
	}
	if len(playbookFiles) == 0 {
		errs = append(errs, fmt.Errorf("%s: no playbooks found", playbooksDir))
	}
	for _, path := range playbookFiles {
		p, err := LoadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if lib.Playbooks[p.ID] != nil {
			errs = append(errs, fmt.Errorf("%s: playbook %q is defined twice", path, p.ID))
			continue
		}
		lib.Playbooks[p.ID] = p
		if err := lib.checkMasterUse(p, ""); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return lib, nil
}

// Check validates a playbook on its own and against the library's master kinds, for a playbook
// that comes from somewhere other than the library folder (e.g. edited in Settings).
func (lib *Library) Check(p *Playbook) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return lib.checkMasterUse(p, "")
}

// checkMasterUse checks every place where a playbook uses master data, or only the places that use
// master kind only, if given.
func (lib *Library) checkMasterUse(p *Playbook, only string) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	fields := map[string]*Field{}
	for i := range p.Fields {
		fields[p.Fields[i].Key] = &p.Fields[i]
	}
	for _, f := range p.Fields {
		if f.Type != "master" || (only != "" && f.Kind != only) {
			continue
		}
		kind := lib.Masters[f.Kind]
		if kind == nil {
			add("field %s: unknown master kind %q", f.Key, f.Kind)
			continue
		}
		for _, target := range slices.Sorted(maps.Keys(f.Prefill)) {
			src, dst := kind.field(f.Prefill[target]), fields[target]
			switch {
			case src == nil:
				add("field %s: prefill: %s has no field %q", f.Key, f.Kind, f.Prefill[target])
			case dst != nil && (src.Type != dst.Type || src.Of != dst.Of):
				add("field %s: prefill: %s.%s is %s, but %s is %s",
					f.Key, f.Kind, src.Key, typeName(src), dst.Key, typeName(dst))
			}
		}
	}
	for _, r := range p.masterRefs {
		kind := lib.Masters[fields[r.field].Kind]
		if only != "" && fields[r.field].Kind != only {
			continue
		}
		if kind != nil && kind.field(r.sub) == nil {
			add("%s: %s has no field %q", r.where, kind.Kind, r.sub)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Name: fmt.Sprintf("playbook %q", p.ID), Problems: problems}
}

// typeName describes a field's type, e.g. "number" or "list of text".
func typeName(f *Field) string {
	if f.Type == "list" {
		return "list of " + f.Of
	}
	return f.Type
}

// yamlFiles lists the .yaml files in dir, sorted by name. A missing folder is an error.
func yamlFiles(dir string) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return filepath.Glob(filepath.Join(dir, "*.yaml"))
}
