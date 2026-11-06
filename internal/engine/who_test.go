// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"seyal/internal/playbook"
)

// whoExample is a made-up desk process (no real business): the desk starts a request, a team takes it
// on, a manager signs it off, and a named reviewer may look again. Every who word is used.
const whoExample = `id: desk-request
version: "1"
description: A request the desk starts and a team handles.
numbering: "R-{seq:4}"
roles: [desk, team, manager]
fields:
  - { key: topic, label: Topic, type: text, required: true }
  - { key: reviewer, label: Reviewer, type: person }
  - { key: notes, label: Notes, type: text, readonly: true }
states: [new, with_team, signed_off, closed]
initial: new
steps:
  new: { owner: starter, returns_to: last }
  with_team: { owner: "role:team", pass_to: ["role:team"], returns_to: last }
transitions:
  - { action: send, from: [new], to: with_team }
  - { action: sign_off, from: [with_team], to: signed_off, who: ["role:manager"] }
  - { action: look_again, from: [with_team], to: new, needs: reason, back: true, who: [holder, "field:reviewer"] }
  - { action: note, from: [with_team], to: with_team, who: [anyone] }
  - { action: close, from: [new, with_team, signed_off], to: closed, needs: reason }
`

func whoPlaybook(t *testing.T) *playbook.Playbook {
	t.Helper()
	p, err := playbook.Parse([]byte(whoExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

// The people: Asha at the desk started it, Ben and Dev are in the team, Mona is a manager, Chitra is an
// admin with no other role. All made up.
var (
	asha   = Actor{Person: Person{ID: "asha", Roles: []string{"desk"}}}
	ben    = Actor{Person: Person{ID: "ben", Roles: []string{"team"}}}
	dev    = Actor{Person: Person{ID: "dev", Roles: []string{"team"}}}
	mona   = Actor{Person: Person{ID: "mona", Roles: []string{"manager"}}}
	chitra = Actor{Person: Person{ID: "chitra", Roles: []string{"admin"}}, Admin: true}
	seyal  = Actor{System: true}
)

func TestAllowed(t *testing.T) {
	p := whoPlaybook(t)
	withTeam := Item{State: "with_team", Team: "team", Starter: "asha"}
	withBen := Item{State: "with_team", Assignee: "ben", Starter: "asha", Fields: Fields{"reviewer": "mona"}}
	draft := Item{State: "new", Assignee: "asha", Starter: "asha"}

	tests := []struct {
		name        string
		item        Item
		who         []string
		a           Actor
		ok, asAdmin bool
	}{
		// The default (no who) is the rule from before: the holder, the starter, or an admin.
		{"default: the starter", withTeam, nil, asha, true, false},
		{"default: anyone in the team that has it", withTeam, nil, dev, true, false},
		{"default: someone else", withTeam, nil, mona, false, false},
		{"default: an admin, as admin", withTeam, nil, chitra, true, true},
		{"default: the person who has it", withBen, nil, ben, true, false},
		{"default: a teammate once Ben has it", withBen, nil, dev, false, false},

		{"holder: the starter is not the holder", withBen, []string{"holder"}, asha, false, false},
		{"holder: an admin passes, as admin", withBen, []string{"holder"}, chitra, true, true},
		{"starter only", draft, []string{"starter"}, asha, true, false},

		// Admins pass holder and starter, but never role: or field: (decision 21).
		{"role: a manager", withTeam, []string{"role:manager"}, mona, true, false},
		{"role: the team is not a manager", withTeam, []string{"role:manager"}, ben, false, false},
		{"role: an admin is not a manager", withTeam, []string{"role:manager"}, chitra, false, false},
		{"field: the reviewer", withBen, []string{"field:reviewer"}, mona, true, false},
		{"field: someone else", withBen, []string{"field:reviewer"}, dev, false, false},
		{"field: an admin is not the reviewer", withBen, []string{"field:reviewer"}, chitra, false, false},
		{"field: empty field means nobody", withTeam, []string{"field:reviewer"}, dev, false, false},
		{"holder or field: the reviewer", withBen, []string{"holder", "field:reviewer"}, mona, true, false},
		{"anyone", withBen, []string{"anyone"}, mona, true, false},

		// Seyal on its own is never limited; an actor that isn't Seyal and has no person always is.
		{"Seyal on its own", withBen, []string{"role:manager"}, seyal, true, false},
		{"no person", withTeam, []string{"anyone"}, Actor{}, false, false},
		{"no person, even as admin", withTeam, nil, Actor{Admin: true}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, asAdmin := Allowed(p, tt.item, tt.who, tt.a)
			if ok != tt.ok || asAdmin != tt.asAdmin {
				t.Errorf("Allowed = %v, %v; want %v, %v", ok, asAdmin, tt.ok, tt.asAdmin)
			}
		})
	}
}

// TestHolderWhoLostTheRole: in a step owned by a team, a person who has the item but no longer has the
// team's role doesn't count as holding it (decision 22). Outside such a step, the role doesn't matter.
func TestHolderWhoLostTheRole(t *testing.T) {
	p := whoPlaybook(t)
	moved := Person{ID: "ben", Roles: []string{"desk"}} // Ben moved from the team to the desk
	if IsHolder(p, Item{State: "with_team", Assignee: "ben"}, moved) {
		t.Error("Ben still counts as holder in the team's step without the team's role")
	}
	if !IsHolder(p, Item{State: "with_team", Assignee: "ben"}, ben.Person) {
		t.Error("Ben with the team's role should hold it")
	}
	if !IsHolder(p, Item{State: "signed_off", Assignee: "ben"}, moved) {
		t.Error("in a step nobody owns, the person it is with holds it, whatever their roles")
	}
	if IsHolder(p, Item{State: "with_team"}, ben.Person) {
		t.Error("an item with nobody holds nobody")
	}
	if ok, _ := Allowed(p, Item{State: "with_team", Assignee: "ben", Starter: "asha"}, nil, Actor{Person: moved}); ok {
		t.Error("Ben without the role may still change the item")
	}
}

// TestHolderOfEscalatedWork: work Seyal escalated to a team counts as that team's only in a step that
// belongs to the team's role (D75, decisions 9 and 10): a manager who isn't an admin may pass it on but
// not take the team's moves; an admin still may. Work merely passed to another team keeps D68's rule.
func TestHolderOfEscalatedWork(t *testing.T) {
	p := whoPlaybook(t)
	manager := Person{ID: "mona", Roles: []string{"manager"}}
	escalated := Item{State: "with_team", Team: "manager", Starter: "asha", Escalated: true}
	if IsHolder(p, escalated, manager) || !Has(escalated, manager) {
		t.Error("a manager should have escalated team work (to pass it on) but not hold it")
	}
	if ok, _ := Allowed(p, escalated, []string{WhoHolder}, Actor{Person: manager}); ok {
		t.Error("a manager who isn't an admin may take the team's move on escalated work")
	}
	if ok, asAdmin := Allowed(p, escalated, []string{WhoHolder}, Actor{Person: manager, Admin: true}); !ok || !asAdmin {
		t.Error("an admin should still be allowed, as an admin")
	}
	passed := escalated
	passed.Escalated = false
	if !IsHolder(p, passed, manager) {
		t.Error("work passed to the manager team (not escalated) should be theirs, as before (D68)")
	}
	own := Item{State: "with_team", Team: "team", Escalated: true}
	if !IsHolder(p, own, ben.Person) {
		t.Error("escalated work with the step's own team is still that team's")
	}
	// A manager who is also in the step's team may do the team's work, escalated or not.
	both := Person{ID: "mira", Roles: []string{"manager", "team"}}
	if !IsHolder(p, escalated, both) {
		t.Error("a manager who is also in the team should hold escalated team work")
	}
	if ok, asAdmin := Allowed(p, escalated, []string{WhoHolder}, Actor{Person: both}); !ok || asAdmin {
		t.Error("a manager who is also in the team should be allowed the team's move, not as an admin")
	}
}

func TestActionsFor(t *testing.T) {
	p := whoPlaybook(t)
	withBen := Item{State: "with_team", Assignee: "ben", Starter: "asha"}
	tests := []struct {
		name string
		a    Actor
		want []string
	}{
		{"the holder", ben, []string{"look_again", "note", "close"}},
		{"the starter", asha, []string{"note", "close"}},
		{"a manager", mona, []string{"sign_off", "note"}},
		{"an admin", chitra, []string{"look_again", "note", "close"}},
		{"Seyal", seyal, []string{"sign_off", "look_again", "note", "close"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ActionsFor(p, withBen, tt.a); !slices.Equal(got, tt.want) {
				t.Errorf("ActionsFor = %v, want %v", got, tt.want)
			}
		})
	}
	if got := Actions(p, withBen); len(got) != 4 {
		t.Errorf("Actions (for chat) = %v, want all four moves from with_team", got)
	}
}

// TestPickThenCheck: Transition is Pick (which line of the move) then Check (the rest); a caller can
// look at the picked line's who words in between.
func TestPickThenCheck(t *testing.T) {
	p := whoPlaybook(t)
	it := Item{State: "with_team", Fields: Fields{"topic": "Printer"}}
	line, err := Pick(p, it, "look_again", nil)
	if err != nil || !line.Back || !slices.Equal(line.Who, []string{"holder", "field:reviewer"}) {
		t.Fatalf("Pick = %+v, %v", line, err)
	}
	if _, err := Check(p, it, line, "", false); !errors.Is(err, ErrReasonNeeded) {
		t.Errorf("Check without a reason = %v", err)
	}
	if to, err := Check(p, it, line, "wrong printer", false); err != nil || to != "new" {
		t.Errorf("Check = %s, %v; want new", to, err)
	}
	if _, err := Pick(p, Item{State: "new"}, "sign_off", nil); !errors.Is(err, ErrNotNow) {
		t.Errorf("Pick from the wrong step = %v", err)
	}
}

// TestFilledCondition: "filled: false" is true while a field is empty, and never "unknown".
func TestFilledCondition(t *testing.T) {
	p := whoPlaybook(t)
	no, yes := false, true
	for _, tt := range []struct {
		value  any
		filled *bool
		want   Result
	}{
		{nil, &no, True}, {"", &no, True}, {[]any{}, &no, True}, {"done", &no, False},
		{nil, &yes, False}, {"done", &yes, True},
	} {
		fields := Fields{}
		if tt.value != nil {
			fields["topic"] = tt.value
		}
		got, err := Eval(p, playbook.Condition{Field: "topic", Filled: tt.filled}, fields, nil)
		if err != nil || got != tt.want {
			t.Errorf("filled %v on %v = %v, %v; want %v", *tt.filled, tt.value, got, err, tt.want)
		}
	}
}

// TestCheckFill: only Seyal filling a field on its own (linked work reporting back) may fill a
// read-only field.
func TestCheckFill(t *testing.T) {
	p := whoPlaybook(t)
	it := Item{State: "with_team"}
	if err := CheckSet(p, it, "notes", "from the study"); err == nil {
		t.Error("a person filled a read-only field")
	}
	if err := CheckFill(p, it, "notes", "from the study"); err != nil {
		t.Errorf("Seyal couldn't fill a read-only field: %v", err)
	}
	if err := CheckFill(p, Item{State: "closed"}, "notes", "late"); err == nil {
		t.Error("Seyal filled a field on a closed item")
	}
}

func TestDescribeWho(t *testing.T) {
	name := func(k string) string { return strings.ToUpper(k[:1]) + k[1:] }
	got := DescribeWho([]string{"holder", "role:manager", "field:reviewer", "anyone"}, name, strings.ToLower)
	if want := "whoever has it or people in Manager or the reviewer or anyone"; got != want {
		t.Errorf("DescribeWho = %q, want %q", got, want)
	}
	if got := DescribeWho(nil, name, name); got != "whoever has it or whoever started it" {
		t.Errorf("DescribeWho(default) = %q", got)
	}
}

// TestHasAndPassTo: Has counts a person who lost the team's role as still having the item (they may
// only pass it on), unlike IsHolder; PassTo is the step's own list, or the default (D68, choice 12).
func TestHasAndPassTo(t *testing.T) {
	p := whoPlaybook(t)
	took := Item{State: "with_team", Assignee: "ben"}
	gone := Person{ID: "ben"} // Ben, after losing the team role
	if !Has(took, gone) || IsHolder(p, took, gone) {
		t.Error("Ben without the team role should still have the item, but not count as its holder")
	}
	team := Item{State: "with_team", Team: "team"}
	if !Has(team, dev.Person) || Has(team, asha.Person) || Has(team, Person{}) {
		t.Error("a team's item is with its people only")
	}
	if Has(took, dev.Person) {
		t.Error("an item Ben took isn't Dev's")
	}

	if got := PassTo(p, "with_team"); !slices.Equal(got, []string{"role:team"}) {
		t.Errorf("PassTo(with_team) = %v; want the step's own list", got)
	}
	if got := PassTo(p, "new"); !slices.Equal(got, []string{WhoAnyone}) {
		t.Errorf("PassTo(new) = %v; want anyone (a step owned by the starter)", got)
	}
	if got := PassTo(p, "signed_off"); !slices.Equal(got, []string{WhoAnyone}) {
		t.Errorf("PassTo(signed_off) = %v; want anyone (no step)", got)
	}
	p.Steps["with_team"] = playbook.Step{Owner: "role:team"} // no pass_to: the owner's role
	if got := PassTo(p, "with_team"); !slices.Equal(got, []string{"role:team"}) {
		t.Errorf("PassTo(with_team) without pass_to = %v; want the owner's role", got)
	}
}

// TestPowerFields: the person fields that decide who gets the item or who may take a move (D68,
// choice 14). The made-up desk process names its reviewer in a move's who; a step owned by a person
// field and an "assign" action count too; a plain person field doesn't.
func TestPowerFields(t *testing.T) {
	p := whoPlaybook(t)
	if got := PowerFields(p); len(got) != 1 || !got["reviewer"] {
		t.Errorf("desk request: %v; want only reviewer", got)
	}
	p.Steps["signed_off"] = playbook.Step{Owner: "field:closer"}
	p.On = map[string][]playbook.Action{"closed": {{Do: "assign", To: "archivist"}, {Do: "copy", From: "topic", To: "notes"}}}
	got := PowerFields(p)
	for _, key := range []string{"reviewer", "closer", "archivist"} {
		if !got[key] {
			t.Errorf("%s isn't a power field: %v", key, got)
		}
	}
	if got["notes"] || got["topic"] || len(got) != 3 {
		t.Errorf("power fields = %v; want reviewer, closer and archivist only", got)
	}
	// A field that says who gets escalated work decides who gets the item too.
	st := p.Steps["with_team"]
	st.EscalateTo = "field:senior"
	p.Steps["with_team"] = st
	if got := PowerFields(p); !got["senior"] || len(got) != 4 {
		t.Errorf("power fields with escalate_to field:senior = %v; want senior too", got)
	}
}
