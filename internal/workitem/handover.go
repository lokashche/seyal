// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Pass on and Take (D68, choices 1, 2, 11-14, 22; build part 3). Every process gets them:
//
//   - Pass gives the item to another person, or to a team (everyone with one role). Whoever has it may
//     pass it (also a person who lost the team's role: that is all they may still do), and admins may,
//     to unstick work. Not the starter, once someone else has it. It goes only to someone the step's
//     pass_to allows (engine.PassTo); an admin may also pass to any other active person, with a notice.
//   - Take gives an item that is with a team to one person of that team. An item nobody has at all
//     (no person, no team, and no starter still here: see Free) may be taken by any person the step's
//     pass_to allows. An item with its starter is theirs: others ask them to pass it on.
//
// Work only goes to people who are active and have a login (choice 12): anyone else would never see
// it. A hand-over never changes the version, so approvals stay (choice 5); the one exception is a step
// owned by a person field, where passing it on sets that field.

// Member is an active person of the business with a login: someone work can be given to.
type Member struct {
	ID, Name string
	Roles    []string
}

// Members lists the business's active people who have a login, by name. tx must come from
// db.WithTenant.
func Members(ctx context.Context, tx pgx.Tx) ([]Member, error) {
	rows, _ := tx.Query(ctx, `
		SELECT p.id::text, p.display_name,
			coalesce(array_agg(r.role_key ORDER BY r.role_key) FILTER (WHERE r.role_key IS NOT NULL), '{}')
		FROM persons p
		LEFT JOIN person_roles r ON r.tenant_id = p.tenant_id AND r.person_id = p.id AND r.revoked_at IS NULL
		WHERE p.tenant_id = current_tenant() AND p.status = 'active' AND p.account_id IS NOT NULL
		GROUP BY p.id, p.display_name
		ORDER BY p.display_name, p.id`)
	members, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Member])
	if err != nil {
		return nil, fmt.Errorf("workitem: members: %w", err)
	}
	return members, nil
}

// Receiver is someone an item may be passed to now: a person, or a team when Person is "".
type Receiver struct {
	Person, Team string
	Name         string // the person's name, or e.g. "the Operations team"
	Empty        bool   // a team nobody is in yet: the work waits there until an admin passes it on
}

// Key is how a form names the receiver: "person:<id>" or "team:<role>".
func (r Receiver) Key() string {
	if r.Person != "" {
		return "person:" + r.Person
	}
	return "team:" + r.Team
}

// TeamName is how a team is called in messages: "the Operations team".
func TeamName(role string, roleNames map[string]string) string {
	return "the " + cmp.Or(roleNames[role], role) + " team"
}

// Receivers lists who may receive the item in its current step (its pass_to), people by name first,
// then teams. Whoever has it now is left out. roleNames turns role keys into the names people know.
func Receivers(p *playbook.Playbook, it Item, members []Member, roleNames map[string]string) []Receiver {
	var people, teams []Receiver
	addPerson := func(m Member) {
		if m.ID != it.AssigneeID && !slices.ContainsFunc(people, func(r Receiver) bool { return r.Person == m.ID }) {
			people = append(people, Receiver{Person: m.ID, Name: m.Name})
		}
	}
	for _, w := range engine.PassTo(p, it.State) {
		kind, name, _ := strings.Cut(w, ":")
		for _, m := range members {
			switch {
			case w == engine.WhoAnyone,
				w == engine.WhoStarter && m.ID == it.CreatedBy,
				kind == "role" && slices.Contains(m.Roles, name),
				kind == "field" && m.ID == fieldPerson(it, name):
				addPerson(m)
			}
		}
		if kind == "role" && !(it.AssigneeID == "" && it.Team == name) &&
			!slices.ContainsFunc(teams, func(r Receiver) bool { return r.Team == name }) {
			empty := !slices.ContainsFunc(members, func(m Member) bool { return slices.Contains(m.Roles, name) })
			teams = append(teams, Receiver{Team: name, Name: TeamName(name, roleNames), Empty: empty})
		}
	}
	if kind, _ := engine.Owner(p, it.State); kind == "field" {
		return people // the step's person field names one person, so a team can't have it (see giveTo)
	}
	return append(people, teams...)
}

func fieldPerson(it Item, key string) string {
	id, _ := it.Fields[key].(string)
	return id
}

// MayPass reports whether a may pass the item on: whoever has it (even after losing the team's role),
// whoever started it while nobody has it (as on My Work), or an admin. asAdmin is true when only being
// an admin allowed it.
func MayPass(it Item, a engine.Actor) (ok, asAdmin bool) {
	switch {
	case a.System:
		return true, false
	case a.ID == "":
		return false, false
	case engine.Has(it.Engine(), a.Person):
		return true, false
	case it.AssigneeID == "" && it.Team == "" && it.CreatedBy == a.ID:
		return true, false
	case a.Admin:
		return true, true
	}
	return false, false
}

// MayTake reports whether a may take the item: it is with a team whose role they have, or it is free
// (see Free) and the step's pass_to allows them.
func MayTake(p *playbook.Playbook, it Item, a engine.Actor, members []Member) bool {
	switch {
	case a.ID == "" || it.AssigneeID != "":
		return false
	case it.Team != "":
		return slices.Contains(a.Roles, it.Team)
	case !Free(it, members):
		return false // its starter has it, so only they act on it (or pass it on)
	}
	return slices.ContainsFunc(Receivers(p, it, members, nil), func(r Receiver) bool { return r.Person == a.ID })
}

// Free reports whether nobody has the item at all: no person, no team, and no starter who is still an
// active member. While nobody else has it, an item is its starter's (MayPass, My Work), so only free
// items may be taken by anyone the step allows: linked work Seyal started, or work whose starter has
// left (D68, choice 13).
func Free(it Item, members []Member) bool {
	return it.AssigneeID == "" && it.Team == "" &&
		(it.CreatedBy == "" || !slices.ContainsFunc(members, func(m Member) bool { return m.ID == it.CreatedBy }))
}

// NobodyCanTake reports whether the item waits where nobody can pick it up: with a team nobody is in,
// or free (see Free) with nobody the step allows. Admins see such items (D68, choice 13).
func NobodyCanTake(p *playbook.Playbook, it Item, members []Member) bool {
	switch {
	case it.AssigneeID != "":
		return false
	case it.Team != "":
		return !slices.ContainsFunc(members, func(m Member) bool { return slices.Contains(m.Roles, it.Team) })
	case !Free(it, members):
		return false
	}
	return !slices.ContainsFunc(Receivers(p, it, members, nil), func(r Receiver) bool { return r.Person != "" })
}

// HolderKey names who has the item, for a form to send back: "person:<id>", "team:<role>" or
// "nobody". If it differs when the form comes back, the item changed hands meanwhile.
func (it Item) HolderKey() string {
	switch {
	case it.AssigneeID != "":
		return "person:" + it.AssigneeID
	case it.Team != "":
		return "team:" + it.Team
	}
	return "nobody"
}

// MaxNote is the longest note a hand-over may carry, in letters.
const MaxNote = 500

// ErrFinished means the item is closed, so it can't change hands.
var ErrFinished = errors.New("this is finished, so it can't be passed on or taken")

// ErrNotHolder means the person may not pass the item on. It counts as ErrNotYours.
var ErrNotHolder error = refusal("only whoever has this item, or an admin, can pass it on; you can still write a note")

// refusal is a refusal in its own words that counts as ErrNotYours (errors.Is), without repeating
// ErrNotYours's own words.
type refusal string

func (r refusal) Error() string { return string(r) }
func (r refusal) Unwrap() error { return ErrNotYours }

// PassOn says whom to give an item to, and why.
type PassOn struct {
	Person string // a person's id, or "" to give it to Team
	Team   string // a role's key
	Note   string // why, in the person's words (optional)
	Via    string // button, panel, chat or voice
	// Seen is the holder the person saw (Item.HolderKey), so a pass based on an old page changes
	// nothing; "" when not known.
	Seen string
}

// Pass gives the item to another person or a team, as whoever has it or an admin, and records it as
// that person with their note (how: pass). notice is a warning that doesn't stop it (an admin passing
// outside the step's list, or a team nobody is in yet). tx must come from db.WithTenant.
func Pass(ctx context.Context, tx pgx.Tx, id string, to PassOn, by ledger.Actor) (it Item, notice string, err error) {
	if it, err = getForUpdate(ctx, tx, id); err != nil {
		return Item{}, "", err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, "", err
	}
	if engine.IsClosed(s.p, it.State) {
		return Item{}, "", ErrFinished
	}
	if to.Seen != "" && to.Seen != it.HolderKey() {
		return Item{}, "", s.changedHands(it, false)
	}
	a, err := s.actor(by)
	if err != nil {
		return Item{}, "", err
	}
	ok, asAdmin := MayPass(it, a)
	if !ok {
		return Item{}, "", ErrNotHolder
	}
	if (to.Person == "") == (to.Team == "") {
		return Item{}, "", errors.New("choose who to pass it to")
	}
	to.Note = strings.TrimSpace(to.Note)
	if utf8.RuneCountInString(to.Note) > MaxNote {
		return Item{}, "", fmt.Errorf("the note is too long; keep it to %d letters", MaxNote)
	}
	roleNames, err := s.roleNames()
	if err != nil {
		return Item{}, "", err
	}
	if (to.Person != "" && to.Person == it.AssigneeID) || (to.Person == "" && it.AssigneeID == "" && to.Team == it.Team) {
		return Item{}, "", fmt.Errorf("it's already with %s", s.holderName(it, roleNames))
	}
	members, err := Members(ctx, tx)
	if err != nil {
		return Item{}, "", err
	}
	list := Receivers(s.p, it, members, roleNames)
	i := slices.IndexFunc(list, func(r Receiver) bool { return r.Person == to.Person && r.Team == to.Team })
	switch {
	case i >= 0 && list[i].Empty:
		notice = fmt.Sprintf("Nobody is in %s yet. It will wait there until an admin passes it on.", list[i].Name)
	case i < 0:
		if to.Person != "" {
			if err := s.mayReceive(to.Person, members); err != nil {
				return Item{}, "", err
			}
		}
		if to.Person == "" || !a.Admin {
			return Item{}, "", s.notReceiver(it, to, list, roleNames)
		}
		// An admin may pass to any active person (choice 12), with a word that it is outside the list.
		asAdmin = true
		name, err := s.personName(to.Person)
		if err != nil {
			return Item{}, "", err
		}
		notice = fmt.Sprintf("Passed to %s as an admin. In this step, work normally goes to %s.", name, s.describePassTo(it, roleNames))
		if kind, role := engine.Owner(s.p, it.State); kind == "role" &&
			!slices.ContainsFunc(members, func(m Member) bool { return m.ID == to.Person && slices.Contains(m.Roles, role) }) {
			// Not in the step's team, so they aren't its holder (engine.IsHolder): they may only pass it on.
			notice += fmt.Sprintf(" %s isn't in %s, so they can only pass it on here.", name, cmp.Or(roleNames[role], role))
		}
	}
	h := handoff{How: HowPass, Note: to.Note, Via: to.Via, From: a.ID}
	if asAdmin {
		h.As = "admin"
	}
	if err := s.giveTo(&it, to.Person, to.Team, by, h); err != nil {
		return Item{}, "", err
	}
	return it, notice, nil
}

// Take gives an item that is with a team (or with nobody) to the person taking it, and records it as
// them (how: take). The team is cleared, so it leaves everyone else's "For my team" (choice 11). seen
// is the holder the person saw (Item.HolderKey), or "" when not known. tx must come from db.WithTenant.
func Take(ctx context.Context, tx pgx.Tx, id, seen, via string, by ledger.Actor) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	if engine.IsClosed(s.p, it.State) {
		return Item{}, ErrFinished
	}
	a, err := s.actor(by)
	if err != nil {
		return Item{}, err
	}
	if a.ID == "" {
		return Item{}, ErrNotYours
	}
	if it.AssigneeID == a.ID {
		return Item{}, errors.New("it's already yours")
	}
	if seen != "" && seen != it.HolderKey() {
		return Item{}, s.changedHands(it, true)
	}
	roleNames, err := s.roleNames()
	if err != nil {
		return Item{}, err
	}
	if it.AssigneeID != "" {
		name, err := s.personName(it.AssigneeID)
		if err != nil {
			return Item{}, err
		}
		return Item{}, fmt.Errorf("%s has it. Ask %s to pass it to you", name, name)
	}
	members, err := Members(s.ctx, s.tx)
	if err != nil {
		return Item{}, err
	}
	if !MayTake(s.p, it, a, members) {
		if it.Team == "" && !Free(it, members) { // with its starter
			if it.CreatedBy == a.ID {
				return Item{}, errors.New("it's already yours")
			}
			name, err := s.personName(it.CreatedBy)
			if err != nil {
				return Item{}, err
			}
			return Item{}, refusal(fmt.Sprintf("%s started it and has it. Ask %s to pass it to you", name, name))
		}
		if it.Team != "" {
			role := cmp.Or(roleNames[it.Team], it.Team)
			return Item{}, fmt.Errorf("only people in %s can take this; it's with %s", role, TeamName(it.Team, roleNames))
		}
		return Item{}, fmt.Errorf("you can't take this in this step; it can go to %s", s.describePassTo(it, roleNames))
	}
	if err := s.mayReceive(a.ID, members); err != nil {
		return Item{}, err
	}
	if err := s.giveTo(&it, a.ID, "", by, handoff{How: HowTake, Via: via}); err != nil {
		return Item{}, err
	}
	return it, nil
}

// giveTo hands the item over as assign does. In a step owned by a person field, the field and the
// holder must always agree, so the field is set first (a field change: the version goes up, choice 5).
func (s session) giveTo(it *Item, person, team string, by ledger.Actor, h handoff) error {
	if kind, name := engine.Owner(s.p, it.State); kind == "field" {
		if person == "" {
			return fmt.Errorf("in this step the work goes to one person (%s), not to a team", s.fieldLabel(name))
		}
		if !engine.SameValue(it.Fields[name], any(person)) {
			state := it.State
			changed, err := s.setFields(*it, engine.Fields{name: person}, by, &h)
			if err != nil {
				return err
			}
			*it = changed
			if it.State != state {
				return nil // the change moved the item to another step, whose owner now has it
			}
		}
	}
	return s.assign(it, person, team, by, h)
}

// ownerFieldChange checks a change to the current step's owner field, the field that says who has the
// item (D68, choice 14). It is a pass on: only whoever has the item or an admin may change it, and only
// to someone who may receive it here. It returns how the item changes hands, or nil when values don't
// change that field (or Seyal changes it on its own).
func (s session) ownerFieldChange(it Item, values engine.Fields, by ledger.Actor) (*handoff, error) {
	kind, name := engine.Owner(s.p, it.State)
	v, ok := values[name]
	if kind != "field" || !ok || by.Kind == ledger.System || engine.SameValue(v, it.Fields[name]) {
		return nil, nil
	}
	a, err := s.actor(by)
	if err != nil {
		return nil, err
	}
	ok, asAdmin := MayPass(it, a)
	if !ok {
		return nil, refusal(fmt.Sprintf("only whoever has this item, or an admin, can change %q, because it says who has the work; "+
			"you can still write a note", s.fieldLabel(name)))
	}
	h := &handoff{How: HowPass, From: a.ID}
	if asAdmin {
		h.As = "admin"
	}
	person, _ := v.(string)
	if person == "" {
		return h, nil // emptied: nobody new gets it, so it stays where it is
	}
	members, err := Members(s.ctx, s.tx)
	if err != nil {
		return nil, err
	}
	roleNames, err := s.roleNames()
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(Receivers(s.p, it, members, roleNames), func(r Receiver) bool { return r.Person == person }) ||
		person == it.AssigneeID {
		return h, nil
	}
	if err := s.mayReceive(person, members); err != nil {
		return nil, err
	}
	if !a.Admin {
		return nil, s.notReceiver(it, PassOn{Person: person}, Receivers(s.p, it, members, roleNames), roleNames)
	}
	h.As = "admin"
	return h, nil
}

// powerFieldChange checks a change to the other person fields that carry power (engine.PowerFields): a
// field that says who gets the item in a later step (for example a Maker that an "assign" action gives
// the work to), or who may take a move or receive the item (D68, choice 14). The current step's owner
// field hands the item over at once, so it is a pass on (see ownerFieldChange). These others only may
// be changed by whoever has the item or an admin; the person named must be active, as for any person
// field. values is what the change really does (see powerChanges), so a maker filled in by picking a
// work type is checked too. Seyal changing a field on its own isn't limited.
func (s session) powerFieldChange(it Item, values engine.Fields, by ledger.Actor) error {
	if by.Kind == ledger.System {
		return nil
	}
	kind, owner := engine.Owner(s.p, it.State)
	power := engine.PowerFields(s.p)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if !power[key] || (kind == "field" && key == owner) || engine.SameValue(values[key], it.Fields[key]) {
			continue
		}
		a, err := s.actor(by)
		if err != nil {
			return err
		}
		if ok, _ := MayPass(it, a); !ok {
			return refusal(fmt.Sprintf("only whoever has this item, or an admin, can change %q, because it decides who gets "+
				"the work; you can still write a note", s.fieldLabel(key)))
		}
		return nil // one check covers every field: it is about who is asking
	}
	return nil
}

// mayReceive says why a person who isn't a member can't receive work: they are no longer active, or
// haven't joined yet (no login), or aren't in this business at all.
func (s session) mayReceive(person string, members []Member) error {
	if slices.ContainsFunc(members, func(m Member) bool { return m.ID == person }) {
		return nil
	}
	var name, status string
	var login bool
	err := s.tx.QueryRow(s.ctx, `
		SELECT display_name, status, account_id IS NOT NULL FROM persons
		WHERE tenant_id = current_tenant() AND id::text = $1`, person).Scan(&name, &status, &login)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errors.New("there is no such person in this business")
	case err != nil:
		return fmt.Errorf("workitem: person: %w", err)
	case status != "active":
		return fmt.Errorf("%s is no longer active, so they can't receive work", name)
	case !login:
		return fmt.Errorf("%s hasn't joined yet (no login), so they can't receive work; an admin can send them a join link in Settings → People", name)
	}
	return nil // a member after all (for example, they joined a moment ago)
}

// notReceiver says that someone may not receive the item in this step, and who may.
func (s session) notReceiver(it Item, to PassOn, list []Receiver, roleNames map[string]string) error {
	name := TeamName(to.Team, roleNames)
	if to.Person != "" {
		var err error
		if name, err = s.personName(to.Person); err != nil {
			return err
		}
	}
	if len(list) == 0 {
		return fmt.Errorf("%s can't receive it in this step, and nobody else can either; an admin can pass it on", name)
	}
	return fmt.Errorf("%s can't receive it in this step. It can go to: %s", name, joinNames(list))
}

// describePassTo says in plain words who the step's work goes to, e.g. "people in Operations".
func (s session) describePassTo(it Item, roleNames map[string]string) string {
	return engine.DescribeWho(engine.PassTo(s.p, it.State),
		func(key string) string { return cmp.Or(roleNames[key], key) },
		func(key string) string {
			if f := s.field(key); f != nil && f.Label != "" {
				return strings.ToLower(f.Label)
			}
			return key
		})
}

// changedHands says the item went to someone else after the person looked at it, so nothing was
// done. taking adds who to ask.
func (s session) changedHands(it Item, taking bool) error {
	roleNames, err := s.roleNames()
	if err != nil {
		return err
	}
	if it.AssigneeID == "" {
		if it.Team != "" {
			return fmt.Errorf("it went to %s a moment ago, so nothing was changed", TeamName(it.Team, roleNames))
		}
		return errors.New("it changed hands a moment ago, so nothing was changed")
	}
	name, err := s.personName(it.AssigneeID)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("it went to %s a moment ago, so nothing was changed", name)
	if it.HandoffFrom == "" { // nobody handed it to them: they took it
		msg = fmt.Sprintf("%s took it a moment ago, so nothing was changed", name)
	}
	if taking {
		msg += fmt.Sprintf(". Ask %s to pass it to you", name)
	}
	return errors.New(msg)
}

// holderName names who has the item: a person, or a team.
func (s session) holderName(it Item, roleNames map[string]string) string {
	if it.AssigneeID != "" {
		if name, err := s.personName(it.AssigneeID); err == nil {
			return name
		}
		return "them"
	}
	if it.Team != "" {
		return TeamName(it.Team, roleNames)
	}
	return "nobody"
}

// roleNames maps the business's role keys to their names.
func (s session) roleNames() (map[string]string, error) {
	rows, _ := s.tx.Query(s.ctx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key, Name string }])
	if err != nil {
		return nil, fmt.Errorf("workitem: roles: %w", err)
	}
	names := make(map[string]string, len(list))
	for _, r := range list {
		names[r.Key] = r.Name
	}
	return names, nil
}

// joinNames lists receivers as people read them: "Ben, Dev, or the Operations team".
func joinNames(list []Receiver) string {
	names := make([]string, len(list))
	for i, r := range list {
		names[i] = r.Name
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}
