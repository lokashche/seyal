// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
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
	// NewEntries are values an entry gets when someone adds it from the chat ("Add it as a new …?"),
	// e.g. { is_new: true } so an admin checks it and processes can treat it as new. Yes/no, text,
	// choice and number details only.
	NewEntries map[string]any `yaml:"new_entries"`
	// Contact says which of the list's text details are an entry's phone number and email address, e.g.
	// { phone: phone, email: email }. A process's share message (ShareMessage.Contact) uses them for
	// its chat link and Email buttons (part 4b-2, D92). Either may be left out.
	Contact *Contact `yaml:"contact"`

	Source []byte   `yaml:"-"` // the definition as written
	Hash   [32]byte `yaml:"-"` // sha256 of Source
}

// Contact names a list's contact details: the keys of its text details that hold a phone number and an
// email address.
type Contact struct {
	Phone string `yaml:"phone"`
	Email string `yaml:"email"`
}

var masterFieldTypes = []string{"text", "number", "money", "date", "bool", "choice", "person", "list"}

// LoadMasterKindFile reads, parses and validates one master kind file.
func LoadMasterKindFile(path string) (*MasterKind, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := ParseMasterKind(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ParseMasterKind reads and validates a master kind definition.
func ParseMasterKind(data []byte) (*MasterKind, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m MasterKind
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("the definition is empty")
		}
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.Source, m.Hash = data, sha256.Sum256(data)
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
		if f.Kind != "" || len(f.Prefill) > 0 || f.Ask != "" || f.FromMessage {
			add("%s: kind, prefill, ask and from_message are only for playbooks", where)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m.NewEntries)) {
		f, v := m.field(k), m.NewEntries[k]
		if f == nil {
			add("new_entries: unknown field %q", k)
			continue
		}
		ok := false
		switch f.Type {
		case "bool":
			_, ok = v.(bool)
		case "text":
			_, ok = v.(string)
		case "choice":
			s, isText := v.(string)
			ok = isText && slices.Contains(f.Options, s)
		case "number":
			switch v.(type) {
			case int, float64:
				ok = true
			}
		}
		if !ok {
			add("new_entries: %s: give a %s value (new entries can only preset yes/no, text, choice and number details)", k, f.Type)
		}
	}
	if c := m.Contact; c != nil {
		if c.Phone == "" && c.Email == "" {
			add("contact: name a phone or an email detail, e.g. { phone: phone, email: email }")
		}
		for _, d := range [][2]string{{"phone", c.Phone}, {"email", c.Email}} {
			if f := m.field(d[1]); d[1] != "" && (f == nil || f.Type != "text") {
				add("contact: %s: %q must be a text detail of this list", d[0], d[1])
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Name: fmt.Sprintf("master kind %q", m.Kind), Problems: problems}
}

// NewEntryFields returns the preset values for an entry added from the chat, as stored values (numbers
// as float64).
func (m *MasterKind) NewEntryFields() map[string]any {
	out := map[string]any{}
	for k, v := range m.NewEntries {
		if n, ok := v.(int); ok {
			v = float64(n)
		}
		out[k] = v
	}
	return out
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
