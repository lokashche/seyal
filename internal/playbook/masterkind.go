// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// MasterKind describes one kind of master data, such as suppliers or price lists. Every master
// item also has a built-in key and name; Fields are its extra details.
type MasterKind struct {
	Kind   string  `yaml:"kind"`
	Label  string  `yaml:"label"`
	Fields []Field `yaml:"fields"`
}

var masterFieldTypes = []string{"text", "number", "money", "date", "bool", "choice", "person", "list"}

// LoadMasterKindFile reads, parses and validates one master kind file.
func LoadMasterKindFile(path string) (*MasterKind, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m MasterKind
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("file is empty")
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Validate checks the master kind on its own.
func (m *MasterKind) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if !keyPattern.MatchString(m.Kind) {
		add("kind: %q must use lowercase letters, digits and _", m.Kind)
	}
	if m.Label == "" {
		add("label: required")
	}
	seen := map[string]bool{}
	for _, f := range m.Fields {
		where := "field " + f.Key
		if !keyPattern.MatchString(f.Key) {
			add("%s: key must use lowercase letters, digits and _", where)
		}
		if seen[f.Key] {
			add("fields: duplicate %q", f.Key)
		}
		seen[f.Key] = true
		if f.Key == "key" || f.Key == "name" {
			add("%s: key and name are built in", where)
		}
		if !slices.Contains(masterFieldTypes, f.Type) {
			add("%s: unknown type %q (use %s)", where, f.Type, strings.Join(masterFieldTypes, ", "))
		}
		if (f.Type == "list") != (f.Of != "") {
			add(`%s: "of" is required for lists and only allowed for lists`, where)
		}
		if (f.Type == "choice") != (len(f.Options) > 0) {
			add(`%s: "options" is required for choices and only allowed for choices`, where)
		}
		if f.Kind != "" || len(f.Prefill) > 0 || f.Ask != "" {
			add("%s: kind, prefill and ask are only for playbooks", where)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Name: fmt.Sprintf("master kind %q", m.Kind), Problems: problems}
}

// field returns the master field with the given key, or nil.
func (m *MasterKind) field(key string) *Field {
	for i := range m.Fields {
		if m.Fields[i].Key == key {
			return &m.Fields[i]
		}
	}
	return nil
}
