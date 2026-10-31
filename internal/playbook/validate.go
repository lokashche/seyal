// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

var (
	keyPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	placeholder  = regexp.MustCompile(`\{([^}]*)\}`)
	numberToken  = regexp.MustCompile(`^(yyyy|yy|seq:[1-9])$`)
	fieldTypes   = []string{"text", "number", "money", "date", "choice", "person", "master", "list"}
	listTypes    = []string{"text", "number", "date", "person"}
	compareTypes = []string{"number", "money", "date"}
)

// ValidationError lists every problem found in one playbook or master kind.
type ValidationError struct {
	Name     string // e.g. playbook "onboarding" or master kind "supplier"
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("playbook %q has %d problem(s):\n  - %s",
		e.Name, len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Validate checks that the playbook makes sense on its own. It reports every problem, not only
// the first, so a file can be fixed in one go. References into master data, such as
// nature_of_work.is_new, are checked later against the master-data kinds.
func (p *Playbook) Validate() error {
	p.masterRefs = nil
	v := validator{p: p, fields: map[string]*Field{}}
	v.basics()
	v.fieldList()
	v.statesAndTransitions()
	v.approvals()
	v.deadlinesAndActions()
	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Name: fmt.Sprintf("playbook %q", p.ID), Problems: v.problems}
}

type validator struct {
	p             *Playbook
	fields        map[string]*Field
	roles, states map[string]bool
	problems      []string
}

func (v *validator) add(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) basics() {
	p := v.p
	if p.ID == "" || strings.TrimSpace(p.ID) != p.ID {
		v.add("id: required, without spaces at the start or end")
	}
	if p.Version == "" || strings.TrimSpace(p.Version) != p.Version {
		v.add("version: required, without spaces at the start or end")
	}
	if p.Description == "" {
		v.add("description: required (it helps people, and later AI, pick the right playbook)")
	}
	seqs := 0
	for _, m := range placeholder.FindAllStringSubmatch(p.Numbering, -1) {
		if !numberToken.MatchString(m[1]) {
			v.add("numbering: unknown placeholder {%s} (use {yyyy}, {yy} or {seq:N})", m[1])
		}
		if strings.HasPrefix(m[1], "seq:") {
			seqs++
		}
	}
	if seqs != 1 {
		v.add(`numbering: must contain {seq:N} exactly once, e.g. "J-{yyyy}-{seq:4}"`)
	}
	v.roles = v.unique("roles", p.Roles)
}

func (v *validator) fieldList() {
	keys := make([]string, len(v.p.Fields))
	for i := range v.p.Fields {
		f := &v.p.Fields[i]
		keys[i] = f.Key
		if v.fields[f.Key] == nil {
			v.fields[f.Key] = f
		}
		where := "field " + f.Key
		if !slices.Contains(fieldTypes, f.Type) {
			v.add("%s: unknown type %q (use %s)", where, f.Type, strings.Join(fieldTypes, ", "))
		}
		if (f.Type == "list") != (f.Of != "") {
			v.add(`%s: "of" is required for lists and only allowed for lists`, where)
		} else if f.Type == "list" && !slices.Contains(listTypes, f.Of) {
			v.add("%s: a list can hold %s, not %q", where, strings.Join(listTypes, ", "), f.Of)
		}
		if (f.Type == "choice") != (len(f.Options) > 0) {
			v.add(`%s: "options" is required for choices and only allowed for choices`, where)
		}
		if (f.Type == "master") != (f.Kind != "") {
			v.add(`%s: "kind" is required for master fields and only allowed for them`, where)
		}
		if len(f.Prefill) > 0 && f.Type != "master" {
			v.add("%s: only master fields can prefill", where)
		}
	}
	v.unique("fields", keys)

	for _, f := range v.p.Fields {
		for _, target := range slices.Sorted(maps.Keys(f.Prefill)) {
			if v.fields[target] == nil {
				v.add("field %s: prefill: unknown field %q", f.Key, target)
			}
		}
	}
	for _, m := range placeholder.FindAllStringSubmatch(v.p.Title, -1) {
		v.fieldRef(m[1], "title")
	}
}

func (v *validator) statesAndTransitions() {
	p := v.p
	if len(p.States) == 0 {
		v.add("states: at least one state is required")
	}
	v.states = v.unique("states", p.States)
	v.state(p.Initial, "initial")

	actions := make([]string, len(p.Transitions))
	for i, t := range p.Transitions {
		actions[i] = t.Action
		where := "transition " + t.Action
		if len(t.From) == 0 {
			v.add(`%s: "from" needs at least one state`, where)
		}
		for _, s := range t.From {
			v.state(s, where+": from")
		}
		v.state(t.To, where+": to")
		if t.When != "" && t.When != "required_filled" {
			v.add(`%s: unknown "when" %q (use required_filled)`, where, t.When)
		}
		if t.Needs != "" && t.Needs != "reason" {
			v.add(`%s: unknown "needs" %q (use reason)`, where, t.Needs)
		}
	}
	v.unique("transitions", actions)
}

func (v *validator) approvals() {
	a := v.p.Approvals
	if a == nil {
		return
	}
	v.state(a.In, "approvals: in")
	v.state(a.OnComplete, "approvals: on_complete")
	v.state(a.OnReject, "approvals: on_reject")
	if len(a.Slots) == 0 {
		v.add("approvals: at least one slot is required")
	}
	names := make([]string, len(a.Slots))
	for i, s := range a.Slots {
		names[i] = s.Slot
		where := "approval slot " + s.Slot
		if !v.roles[s.Role] {
			v.add("%s: unknown role %q (declare it under roles)", where, s.Role)
		}
		if s.RequiredIf != nil {
			v.condition(*s.RequiredIf, where+": required_if")
		}
	}
	v.unique("approval slots", names)
}

func (v *validator) condition(c Condition, where string) {
	ops := map[string]*Value{"eq": c.Eq, "ne": c.Ne, "lt": c.Lt, "gt": c.Gt}
	var used []string
	for _, name := range []string{"eq", "ne", "lt", "gt"} {
		if ops[name] != nil {
			used = append(used, name)
		}
	}
	kinds := 0
	for _, present := range []bool{len(c.Any) > 0, len(c.All) > 0, c.Field != ""} {
		if present {
			kinds++
		}
	}
	if kinds != 1 {
		v.add("%s: use exactly one of any, all or field", where)
		return
	}
	for i, sub := range c.Any {
		v.condition(sub, fmt.Sprintf("%s: any[%d]", where, i))
	}
	for i, sub := range c.All {
		v.condition(sub, fmt.Sprintf("%s: all[%d]", where, i))
	}
	if c.Field == "" {
		if len(used) > 0 {
			v.add("%s: eq, ne, lt and gt need a field", where)
		}
		return
	}
	if len(used) != 1 {
		v.add("%s: field %q needs exactly one of eq, ne, lt, gt", where, c.Field)
		return
	}

	op, val := used[0], ops[used[0]]
	f := v.fieldRef(c.Field, where)
	var other *Field
	if val.Field != "" {
		other = v.fieldRef(val.Field, where)
	}
	if op == "lt" || op == "gt" {
		for _, x := range []*Field{f, other} {
			if x != nil && !slices.Contains(compareTypes, x.Type) {
				v.add("%s: %s only works on number, money or date fields, not %s (%s)", where, op, x.Key, x.Type)
			}
		}
	}
	if f != nil && f.Type == "choice" && val.Field == "" {
		if s, ok := val.Literal.(string); !ok || !slices.Contains(f.Options, s) {
			v.add("%s: %v is not one of the options of %s (%s)", where, val.Literal, f.Key, strings.Join(f.Options, ", "))
		}
	}
}

func (v *validator) deadlinesAndActions() {
	for _, d := range v.p.Deadlines {
		if f := v.fieldRef(d.DueFrom, "deadline"); f != nil && f.Type != "date" {
			v.add("deadline: due_from must be a date field, %s is %s", f.Key, f.Type)
		}
	}
	for _, state := range slices.Sorted(maps.Keys(v.p.On)) {
		where := "on " + state
		v.state(state, where)
		for _, a := range v.p.On[state] {
			switch a.Do {
			case "post":
				if a.Text == "" || a.To != "" {
					v.add(`%s: post needs text and no "to"`, where)
				}
				for _, m := range placeholder.FindAllStringSubmatch(a.Text, -1) {
					v.fieldRef(m[1], where+": post")
				}
			case "assign":
				if a.Text != "" {
					v.add("%s: assign takes no text", where)
				}
				if f := v.fieldRef(a.To, where+": assign"); f != nil && f.Type != "person" {
					v.add("%s: assign needs a person field, %s is %s", where, f.Key, f.Type)
				}
			default:
				v.add("%s: unknown action %q (use post or assign)", where, a.Do)
			}
		}
	}
}

// unique reports keys that are badly formed or repeated, and returns the set of keys.
func (v *validator) unique(what string, keys []string) map[string]bool {
	seen := map[string]bool{}
	for _, k := range keys {
		if !keyPattern.MatchString(k) {
			v.add("%s: %q must use lowercase letters, digits and _", what, k)
		}
		if seen[k] {
			v.add("%s: duplicate %q", what, k)
		}
		seen[k] = true
	}
	return seen
}

func (v *validator) state(s, where string) {
	if !v.states[s] {
		v.add("%s: unknown state %q", where, s)
	}
}

// fieldRef checks a reference such as "fee_agreed" or "nature_of_work.is_new". It returns the
// field for a plain reference, or nil for a master-data reference or an unknown field.
func (v *validator) fieldRef(ref, where string) *Field {
	key, sub, dotted := strings.Cut(ref, ".")
	f := v.fields[key]
	switch {
	case f == nil:
		v.add("%s: unknown field %q", where, key)
	case dotted && f.Type != "master":
		v.add("%s: %q is not a master field, so %q is not possible", where, key, ref)
	case dotted && !keyPattern.MatchString(sub):
		v.add("%s: bad master field name in %q", where, ref)
	case dotted:
		v.p.masterRefs = append(v.p.masterRefs, masterRef{field: key, sub: sub, where: where})
	case !dotted:
		return f
	}
	return nil
}
