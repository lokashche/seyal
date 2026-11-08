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
	return fmt.Sprintf("%s has %d problem(s):\n  - %s", // Name already says what it is
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
	v.steps()
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
		if f.FromMessage && f.Type != "text" {
			v.add("%s: only text fields can be filled from the message", where)
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

	var actions []string
	open := map[string]bool{} // "action/from" listed with no condition: a default
	for _, t := range p.Transitions {
		if !slices.Contains(actions, t.Action) {
			actions = append(actions, t.Action)
		}
		where := "transition " + t.Action
		for _, s := range t.From {
			if open[t.Action+"/"+s] {
				v.add("%s: from %s it already has a move without a condition, which always wins; put moves with if first", where, s)
			}
			if t.If == nil {
				open[t.Action+"/"+s] = true
			}
		}
		if t.If != nil {
			v.condition(*t.If, where+": if")
		}
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
		if t.By != "" && t.By != "seyal" {
			v.add(`%s: unknown "by" %q (use seyal, or leave it out)`, where, t.By)
		}
		if t.By == "seyal" && len(t.Who) > 0 {
			v.add(`%s: "who" doesn't go with "by: seyal" (only Seyal takes that move)`, where)
		}
		v.whoWords(t.Who, where+": who", false)
		if first := v.firstLine(t.Action); !slices.Equal(first.Who, t.Who) || first.Back != t.Back {
			v.add(`%s: every line of one move needs the same "who" and "back"`, where)
		}
		if t.Back {
			v.back(t, where)
		}
	}
	v.unique("transitions", actions)
}

// firstLine returns the first line of a move. One move can have several lines (picked by "if").
func (v *validator) firstLine(action string) Transition {
	for _, t := range v.p.Transitions {
		if t.Action == action {
			return t
		}
	}
	return Transition{}
}

// back checks a move that sends work back: it needs a reason, and it may not go into the approvals
// step, the step after the approvals, or a step with no way out, because going back skips the checks
// those steps need.
func (v *validator) back(t Transition, where string) {
	if t.Needs != "reason" {
		v.add(`%s: "back: true" needs "needs: reason" (people say why it goes back)`, where)
	}
	a := v.p.Approvals
	switch {
	case !v.states[t.To]:
	case a != nil && t.To == a.In:
		v.add("%s: a move that sends work back can't go into %s, the approvals step", where, t.To)
	case a != nil && t.To == a.OnComplete:
		v.add("%s: a move that sends work back can't go into %s, the step after the approvals", where, t.To)
	case v.p.closed(t.To):
		v.add("%s: a move that sends work back can't go into %s, which has no way out", where, t.To)
	}
}

// closed reports whether state has no way out (no move from it, and it isn't the approvals step).
// It is the same rule as engine.IsClosed, which this package can't use.
func (p *Playbook) closed(state string) bool {
	if p.Approvals != nil && p.Approvals.In == state {
		return false
	}
	return !slices.ContainsFunc(p.Transitions, func(t Transition) bool { return slices.Contains(t.From, state) })
}

// whoWords checks a list of who words: holder, starter, anyone, role:<role> or field:<person field>.
// A list that says who may receive the item (passTo) can't name the holder.
func (v *validator) whoWords(words []string, where string, passTo bool) {
	for _, w := range words {
		kind, name, named := strings.Cut(w, ":")
		switch {
		case w == "holder" && passTo:
			v.add("%s: holder can't receive the item, it already has it (use starter, role:<role>, field:<person field> or anyone)", where)
		case w == "holder" || w == "starter" || w == "anyone":
		case named && kind == "role":
			if !v.roles[name] {
				v.add("%s: unknown role %q (declare it under roles)", where, name)
			}
		case named && kind == "field" && strings.Contains(name, "."):
			v.add("%s: %q must name a person field of this process", where, w)
		case named && kind == "field":
			if f := v.fieldRef(name, where); f != nil && f.Type != "person" {
				v.add("%s: %s needs a person field, %s is %s", where, w, f.Key, f.Type)
			}
		default:
			v.add("%s: unknown word %q (use holder, starter, anyone, role:<role> or field:<person field>)", where, w)
		}
	}
}

func (v *validator) steps() {
	for _, state := range slices.Sorted(maps.Keys(v.p.Steps)) {
		st, where := v.p.Steps[state], "step "+state
		v.state(state, where)
		kind, name, _ := strings.Cut(st.Owner, ":")
		switch {
		case st.Owner == "" || st.Owner == "starter":
		case kind == "role":
			if !v.roles[name] {
				v.add("%s: owner: unknown role %q (declare it under roles)", where, name)
			}
		case kind == "field":
			if f := v.fieldRef(name, where+": owner"); f != nil && f.Type != "person" {
				v.add("%s: owner needs a person field, %s is %s", where, f.Key, f.Type)
			}
		default:
			v.add(`%s: owner %q: use starter, role:<role> or field:<person field>`, where, st.Owner)
		}
		v.whoWords(st.PassTo, where+": pass_to", true)
		if st.ReturnsTo != "" && st.ReturnsTo != "last" {
			v.add(`%s: unknown "returns_to" %q (use last, or leave it out)`, where, st.ReturnsTo)
		}
		for _, list := range []struct {
			name string
			keys []string
		}{{"asks", st.Asks}, {"needs", st.Needs}} {
			for _, key := range list.keys {
				if f := v.fieldRef(key, where+": "+list.name); f != nil && f.Readonly && list.name == "asks" {
					v.add("%s: asks: %s is filled automatically, so Seyal can't ask for it", where, key)
				}
			}
		}
	}
}

func (v *validator) approvals() {
	a := v.p.Approvals
	if a == nil {
		return
	}
	v.state(a.In, "approvals: in")
	v.state(a.OnComplete, "approvals: on_complete")
	v.state(a.OnReject, "approvals: on_reject")
	// A refusal that sends work back works like a move marked back, so it may go only where such a
	// move may go (see back).
	if a.OnRejectBack && v.states[a.OnReject] {
		switch {
		case a.OnReject == a.In:
			v.add("approvals: on_reject_back: a refusal that sends work back can't go into %s, the approvals step", a.OnReject)
		case a.OnReject == a.OnComplete:
			v.add("approvals: on_reject_back: a refusal that sends work back can't go into %s, the step after the approvals", a.OnReject)
		case v.p.closed(a.OnReject):
			v.add("approvals: on_reject_back: a refusal that sends work back can't go into %s, which has no way out", a.OnReject)
		}
	}
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
	if c.Filled != nil {
		used = append(used, "filled")
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
		if len(used) > 0 || c.IfMissing != nil {
			v.add("%s: eq, ne, lt, gt, filled and if_missing need a field", where)
		}
		return
	}
	if len(used) != 1 {
		v.add("%s: field %q needs exactly one of eq, ne, lt, gt, filled", where, c.Field)
		return
	}
	if used[0] == "filled" {
		v.fieldRef(c.Field, where)
		if c.IfMissing != nil {
			v.add("%s: if_missing doesn't go with filled (filled already says what an empty field means)", where)
		}
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
			if a.IfEmpty && a.Do != "copy" {
				v.add("%s: if_empty only goes with copy", where)
			}
			switch a.Do {
			case "post":
				if a.Text == "" || a.To != "" || a.From != "" {
					v.add(`%s: post needs text and no "to" or "from"`, where)
				}
				for _, m := range placeholder.FindAllStringSubmatch(a.Text, -1) {
					v.fieldRef(m[1], where+": post")
				}
			case "assign":
				if a.Text != "" || a.From != "" {
					v.add(`%s: assign takes no text or "from"`, where)
				}
				if f := v.fieldRef(a.To, where+": assign"); f != nil && f.Type != "person" {
					v.add("%s: assign needs a person field, %s is %s", where, f.Key, f.Type)
				}
			case "copy":
				from, to := v.fieldRef(a.From, where+": copy from"), v.fieldRef(a.To, where+": copy to")
				if a.Text != "" {
					v.add("%s: copy takes no text", where)
				}
				if from != nil && to != nil && (from.Type != to.Type || from.Of != to.Of) {
					v.add("%s: copy from %s (%s) to %s (%s): the types must match", where, from.Key, from.Type, to.Key, to.Type)
				}
			case "start":
				if strings.TrimSpace(a.Process) == "" || a.Text != "" || a.To != "" || a.From != "" || a.Action != "" {
					v.add(`%s: start needs "process" (and optionally "copy"), nothing else`, where)
				}
				for _, child := range slices.Sorted(maps.Keys(a.Copy)) {
					v.fieldRef(a.Copy[child], where+": start: copy")
				}
			case "tell_parent":
				if strings.TrimSpace(a.Action) == "" || a.Text != "" || a.To != "" || a.From != "" || a.Process != "" {
					v.add(`%s: tell_parent needs "action" (and optionally "copy"), nothing else`, where)
				}
				for _, parent := range slices.Sorted(maps.Keys(a.Copy)) {
					v.fieldRef(a.Copy[parent], where+": tell_parent: copy")
				}
			default:
				v.add("%s: unknown action %q (use post, assign, copy, start or tell_parent)", where, a.Do)
			}
		}
	}
}

// ReservedMoves are Seyal's own chat words for moving work between people (D68). A new process may
// not name a move after one of them, so a typed "take it" always means the same thing.
var ReservedMoves = []string{"pass", "pass_on", "take", "give_back", "send_back"}

// CheckNew runs the checks that apply only to a version being registered for the first time. Rule:
// a check added after versions were stored goes here, never into Validate, because Validate also
// runs when a stored version is opened, and a business's work must keep opening after an upgrade.
func (p *Playbook) CheckNew() error {
	var problems []string
	// Passing an item on in a step owned by a person field changes that field, so the version goes up;
	// in the approvals step, or the step after it, that would throw away approvals already given.
	if a := p.Approvals; a != nil {
		for _, state := range []string{a.In, a.OnComplete} {
			if kind, _, _ := strings.Cut(p.Steps[state].Owner, ":"); kind == "field" {
				problems = append(problems, fmt.Sprintf(
					"step %s: owner: the approvals step and the step after it can't be owned by a person field (use starter or role:<role>)", state))
			}
		}
	}
	var seen []string
	for _, t := range p.Transitions {
		if slices.Contains(ReservedMoves, t.Action) && !slices.Contains(seen, t.Action) {
			seen = append(seen, t.Action)
			problems = append(problems, fmt.Sprintf(
				"transition %s: %q is one of Seyal's own words (%s); give the move another name",
				t.Action, t.Action, strings.Join(ReservedMoves, ", ")))
		}
	}
	// "field:<f>" in a move's who or a step's pass_to is allowed again (D74). Such a field, and one a
	// step's escalate_to names, carries power
	// (engine.PowerFields): only whoever has the item or an admin may change it (D72), and a value the AI
	// model or a voice note gives it needs the person's Yes first (D74).
	problems = append(problems, p.timingProblems()...)
	problems = append(problems, p.scheduleProblems()...)
	problems = append(problems, p.shareProblems()...)
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Name: fmt.Sprintf("playbook %q", p.ID), Problems: problems}
}

// timingProblems checks the steps' timing (D75, M6): remind_after, escalate_after and escalate_to. It
// is new in Phase 2, so it runs only for a version being added (see CheckNew).
func (p *Playbook) timingProblems() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	fields := map[string]Field{}
	for _, f := range p.Fields {
		fields[f.Key] = f
	}
	for _, state := range slices.Sorted(maps.Keys(p.Steps)) {
		st, where := p.Steps[state], "step "+state
		if st.RemindAfter == "" && st.EscalateAfter == "" && st.EscalateTo == "" {
			continue
		}
		owner, ownerName, _ := strings.Cut(st.Owner, ":")
		switch {
		case p.closed(state):
			add("%s: has no way out, so nothing waits there to be chased (remove remind_after, escalate_after and escalate_to)", where)
			continue
		case st.RemindAfter == "":
			add("%s: escalate_after and escalate_to need remind_after: Seyal always reminds before it escalates", where)
			continue
		}
		remind, err := Days(st.RemindAfter)
		if err != nil {
			add("%s: remind_after: %v", where, err)
		}
		if st.EscalateAfter == "" {
			if st.EscalateTo != "" {
				add("%s: escalate_to needs escalate_after (how many working days before Seyal hands it on)", where)
			}
			continue
		}
		switch {
		case p.Approvals != nil && state == p.Approvals.In:
			add("%s: no escalation in the approvals step: approvals belong to roles, so handing the item on changes nothing (keep only remind_after)", where)
			continue
		case owner == "field":
			add("%s: no escalation in a step owned by a person field (%s): it would change that field to someone else (keep only remind_after)", where, ownerName)
			continue
		}
		escalate, err := Days(st.EscalateAfter)
		if err != nil {
			add("%s: escalate_after: %v", where, err)
		} else if remind > 0 && escalate <= remind {
			add("%s: escalate_after (%s) must be longer than remind_after (%s)", where, st.EscalateAfter, st.RemindAfter)
		}
		kind, name, _ := strings.Cut(st.EscalateTo, ":")
		switch {
		case st.EscalateTo == "":
			add("%s: escalate_after needs escalate_to: role:<role> or field:<person field>", where)
		case kind == "role" && !slices.Contains(p.Roles, name):
			add("%s: escalate_to: unknown role %q (declare it under roles)", where, name)
		case kind == "role" && owner == "role" && name == ownerName:
			add("%s: escalate_to can't be the step's own team (%s): that would take the work from its holder and give it to the same team", where, st.EscalateTo)
		case kind == "field" && fields[name].Type != "person":
			add("%s: escalate_to: %q must name a person field of this process", where, st.EscalateTo)
		case kind != "role" && kind != "field":
			add("%s: escalate_to %q: use role:<role> or field:<person field>", where, st.EscalateTo)
		}
	}
	return problems
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
