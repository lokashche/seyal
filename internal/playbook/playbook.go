// SPDX-License-Identifier: AGPL-3.0-or-later

// Package playbook reads playbooks: YAML files that describe one kind of work (its fields,
// states, approvals, deadlines and actions). The engine stays generic; playbooks hold the
// business words.
package playbook

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// Playbook is one parsed playbook file.
type Playbook struct {
	ID          string              `yaml:"id"`
	Version     string              `yaml:"version"`
	Description string              `yaml:"description"`
	Examples    []string            `yaml:"examples"`
	Numbering   string              `yaml:"numbering"` // e.g. "J-{yyyy}-{seq:4}"
	Title       string              `yaml:"title"`     // e.g. "{who} — {what}", using field keys
	Roles       []string            `yaml:"roles"`
	Fields      []Field             `yaml:"fields"`
	States      []string            `yaml:"states"`
	Initial     string              `yaml:"initial"`
	Transitions []Transition        `yaml:"transitions"`
	Steps       map[string]Step     `yaml:"steps"` // state → who owns it and what Seyal asks in it
	Approvals   *Approvals          `yaml:"approvals"`
	Deadlines   []Deadline          `yaml:"deadlines"`
	On          map[string][]Action `yaml:"on"` // state → actions to run on entering it

	Source     []byte      `yaml:"-"` // the exact file contents
	Hash       [32]byte    `yaml:"-"` // sha256 of Source, used to spot changed files
	masterRefs []masterRef // master-data references, filled by Validate

}

// Field is one piece of information a work item holds.
type Field struct {
	Key      string            `yaml:"key"`
	Label    string            `yaml:"label"`
	Type     string            `yaml:"type"`    // text, number, money, date, choice, person, master, list
	Of       string            `yaml:"of"`      // element type, for list
	Kind     string            `yaml:"kind"`    // master-data kind, for master
	Options  []string          `yaml:"options"` // allowed values, for choice
	Required bool              `yaml:"required"`
	Readonly bool              `yaml:"readonly"`
	Ask      string            `yaml:"ask"`     // question Seyal asks when the field is missing
	Prefill  map[string]string `yaml:"prefill"` // this playbook's field → master field
}

// Transition moves a work item from one state to another when someone takes an action. The same
// action may be listed more than once with an If condition: the first one whose condition is true
// is taken, and one without If is the default.
type Transition struct {
	Action string     `yaml:"action"`
	From   []string   `yaml:"from"`
	To     string     `yaml:"to"`
	When   string     `yaml:"when"`  // "" or "required_filled"
	Needs  string     `yaml:"needs"` // "" or "reason"
	If     *Condition `yaml:"if"`    // optional: only when this is true
	By     string     `yaml:"by"`    // "" (anyone) or "seyal": only Seyal takes it, e.g. when linked work is done
}

// Step says, for one state, who has the item and what Seyal asks for. States without a step keep
// the item with whoever has it, and Seyal asks for the required fields.
type Step struct {
	// Owner is who the item goes to on entering the state: "starter" (who started it),
	// "role:<role>" (anyone with the role), or "field:<person field>".
	Owner string   `yaml:"owner"`
	Asks  []string `yaml:"asks"`  // fields Seyal asks for in this state, in this order
	Needs []string `yaml:"needs"` // fields a required_filled move out of this state needs
}

// Approvals says who must approve, while the item is in state In.
type Approvals struct {
	In         string `yaml:"in"`
	OnComplete string `yaml:"on_complete"`
	OnReject   string `yaml:"on_reject"`
	Slots      []Slot `yaml:"slots"`
}

// Slot is one approval that must be given by someone holding Role.
type Slot struct {
	Slot       string     `yaml:"slot"`
	Role       string     `yaml:"role"`
	RequiredIf *Condition `yaml:"required_if"` // nil means always required
}

// Condition is a yes/no question about a work item's fields. Use Any or All to combine
// conditions, or Field with exactly one comparison.
type Condition struct {
	Any   []Condition `yaml:"any"`
	All   []Condition `yaml:"all"`
	Field string      `yaml:"field"` // field key, or key.subkey for a master field
	Eq    *Value      `yaml:"eq"`
	Ne    *Value      `yaml:"ne"`
	Lt    *Value      `yaml:"lt"`
	Gt    *Value      `yaml:"gt"`
}

// Value is what a field is compared with: a literal (fail, true, 100) or another field
// ({ field: standard_fee }).
type Value struct {
	Literal any
	Field   string
}

// UnmarshalYAML reads a Value from either form.
func (v *Value) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return n.Decode(&v.Literal)
	}
	if len(n.Content) != 2 || n.Content[0].Value != "field" {
		return fmt.Errorf("line %d: a value must be a plain value or { field: name }", n.Line)
	}
	v.Field = n.Content[1].Value
	return nil
}

// Deadline sets a work item's due date from one of its date fields.
type Deadline struct {
	DueFrom string `yaml:"due_from"`
}

// Action is something Seyal does automatically, e.g. post a message or assign someone.
type Action struct {
	Do      string            `yaml:"do"`      // post, assign, copy, start, tell_parent
	Text    string            `yaml:"text"`    // for post
	To      string            `yaml:"to"`      // person field, for assign; the field to fill, for copy
	From    string            `yaml:"from"`    // the field to copy from, for copy
	Process string            `yaml:"process"` // for start: the id of the process to start as linked work
	Action  string            `yaml:"action"`  // for tell_parent: the parent's action to take
	Copy    map[string]string `yaml:"copy"`    // for start: child field → this field; for tell_parent: parent field → this field
}

// Parse reads one playbook from YAML. Unknown keys are errors, so typos are caught.
func Parse(data []byte) (*Playbook, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Playbook
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	p.Source = data
	p.Hash = sha256.Sum256(data)
	return &p, nil
}

// LoadFile reads and parses one playbook file.
func LoadFile(path string) (*Playbook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

type masterRef struct {
	field string // the master field key
	sub   string // the subfield key, e.g. "name"
	where string // e.g. "prefill: nature_of_work"
}
