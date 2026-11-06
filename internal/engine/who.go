// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"slices"
	"strings"

	"seyal/internal/playbook"
)

// The who words (D68) say which people a process means: on a move, who may take it ("who:"); on a
// step, who may receive the item there ("pass_to:", see PassTo).
//
//	holder        whoever has the item now: the person, or anyone in the team that has it. In a
//	              step owned by a role, a person counts only while they still have that role.
//	starter       whoever started the item
//	role:<r>      anyone with role r
//	field:<f>     the person named in person field f
//	anyone        any person in the business
const (
	WhoHolder  = "holder"
	WhoStarter = "starter"
	WhoAnyone  = "anyone"
)

// DefaultWho is who may take a move when the process doesn't say: whoever has the item and whoever
// started it. Admins pass both words, so this is the rule from before who words existed (D55).
var DefaultWho = []string{WhoHolder, WhoStarter}

// Actor is someone about to change an item, as the who words see them.
type Actor struct {
	// System is Seyal acting on its own (a step's rule, linked work reporting back). The who words
	// don't limit it.
	System bool
	// Person is who acts, with their roles. For Seyal acting for a person, it is that person.
	Person
	// Admin may unstick any work: an admin passes the holder and starter words, but never role: or
	// field: (D68, choice 21).
	Admin bool
}

// Allowed reports whether a may take a move limited by who (nil or empty: DefaultWho). asAdmin is
// true when only being an admin allowed it; the change then records that. Someone who isn't Seyal
// and has no person is never allowed: that would be a mistake in the calling code.
func Allowed(p *playbook.Playbook, it Item, who []string, a Actor) (ok, asAdmin bool) {
	if a.System {
		return true, false
	}
	if a.ID == "" {
		return false, false
	}
	if len(who) == 0 {
		who = DefaultWho
	}
	for _, w := range who {
		if is(p, it, a.Person, w) {
			return true, false
		}
	}
	if a.Admin && (slices.Contains(who, WhoHolder) || slices.Contains(who, WhoStarter)) {
		return true, true
	}
	return false, false
}

// is reports whether person fits one who word on the item.
func is(p *playbook.Playbook, it Item, person Person, word string) bool {
	kind, name, _ := strings.Cut(word, ":")
	switch {
	case word == WhoHolder:
		return IsHolder(p, it, person)
	case word == WhoStarter:
		return it.Starter != "" && it.Starter == person.ID
	case word == WhoAnyone:
		return true
	case kind == "role":
		return slices.Contains(person.Roles, name)
	case kind == "field":
		id, _ := it.Fields[name].(string)
		return id != "" && id == person.ID
	}
	return false
}

// IsHolder reports whether person has the item now: it is with them, or with a team whose role they
// have. In a step owned by a role, a person who has the item but lost that role no longer counts
// (D68, choice 22): they may only pass it on (build part 3). Work Seyal escalated to a team counts as
// the team's only in a step that belongs to that team's role (D75, decisions 9 and 10): an Owner who
// isn't an admin may pass escalated Operations work on, but not do the Operations work. An Owner who is
// also in Operations still may.
func IsHolder(p *playbook.Playbook, it Item, person Person) bool {
	if person.ID == "" {
		return false
	}
	kind, role := Owner(p, it.State)
	if it.Assignee != "" {
		if it.Assignee != person.ID {
			return false
		}
		return kind != "role" || slices.Contains(person.Roles, role)
	}
	if it.Escalated && !(kind == "role" && (role == it.Team || slices.Contains(person.Roles, role))) {
		return false
	}
	return it.Team != "" && slices.Contains(person.Roles, it.Team)
}

// Has reports whether the item is with person: assigned to them, or with a team whose role they
// have. Unlike IsHolder, a person who has the item but lost the step's role still has it, because
// they may still pass it on or give it back to the team (D68, choice 22).
func Has(it Item, person Person) bool {
	if person.ID == "" {
		return false
	}
	if it.Assignee != "" {
		return it.Assignee == person.ID
	}
	return it.Team != "" && slices.Contains(person.Roles, it.Team)
}

// PassTo returns who may receive an item in state, in who words: the step's own pass_to, or the
// default. In a step owned by a role the default is that role (its people, and its team); in any other
// state it is anyone (people only, not teams) (D68, choice 12).
func PassTo(p *playbook.Playbook, state string) []string {
	if st, ok := p.Steps[state]; ok && len(st.PassTo) > 0 {
		return st.PassTo
	}
	if kind, role := Owner(p, state); kind == "role" {
		return []string{"role:" + role}
	}
	return []string{WhoAnyone}
}

// WhoFor returns the who words of action from state (every line of one move has the same ones; the
// file check makes sure), or nil for the default.
func WhoFor(p *playbook.Playbook, state, action string) []string {
	for _, t := range p.Transitions {
		if t.Action == action && slices.Contains(t.From, state) {
			return t.Who
		}
	}
	return nil
}

// ActionsFor is Actions, keeping only the moves a may take: for buttons and hints. Chat still
// matches a typed word against every move, so it can say why someone may not take it.
func ActionsFor(p *playbook.Playbook, it Item, a Actor) []string {
	var out []string
	for _, action := range Actions(p, it) {
		if ok, _ := Allowed(p, it, WhoFor(p, it.State, action), a); ok {
			out = append(out, action)
		}
	}
	return out
}

// PowerFields returns the person fields that carry power (D68, choice 14): a field that says who gets
// the item (a step's owner "field:<f>", its escalate_to "field:<f>", or an "assign" action), or who
// may take a move or receive the item ("field:<f>" in who or pass_to). Only whoever has the item or an
// admin may change them.
func PowerFields(p *playbook.Playbook) map[string]bool {
	power := map[string]bool{}
	add := func(words ...string) {
		for _, w := range words {
			if name, ok := strings.CutPrefix(w, "field:"); ok {
				power[name] = true
			}
		}
	}
	for _, st := range p.Steps {
		add(st.Owner, st.EscalateTo)
		add(st.PassTo...)
	}
	for _, t := range p.Transitions {
		add(t.Who...)
	}
	for _, actions := range p.On {
		for _, a := range actions {
			if a.Do == "assign" && a.To != "" {
				power[a.To] = true
			}
		}
	}
	return power
}

// DescribeWho says in plain words who the who words mean, e.g. "whoever has it or whoever started
// it". roleName and fieldName turn keys into the names people know.
func DescribeWho(who []string, roleName, fieldName func(string) string) string {
	if len(who) == 0 {
		who = DefaultWho
	}
	parts := make([]string, 0, len(who))
	for _, w := range who {
		kind, name, _ := strings.Cut(w, ":")
		switch {
		case w == WhoHolder:
			parts = append(parts, "whoever has it")
		case w == WhoStarter:
			parts = append(parts, "whoever started it")
		case w == WhoAnyone:
			parts = append(parts, "anyone")
		case kind == "role":
			parts = append(parts, "people in "+roleName(name))
		case kind == "field":
			parts = append(parts, "the "+fieldName(name))
		default:
			parts = append(parts, w)
		}
	}
	return strings.Join(parts, " or ")
}
