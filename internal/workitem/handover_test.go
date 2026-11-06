// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"slices"
	"testing"

	"seyal/internal/engine"
	"seyal/internal/playbook"
)

// handExample is a made-up process (no real business): the desk starts it, a team handles it, and in
// the last step one named person checks it.
const handExample = `id: hand-example
version: "1"
description: The desk starts it, a team handles it, a named person checks it.
numbering: "H-{seq:4}"
roles: [desk, team]
fields:
  - { key: topic, label: Topic, type: text }
  - { key: checker, label: Checker, type: person }
states: [new, with_team, checking, waiting, done]
initial: new
steps:
  new: { owner: starter }
  with_team: { owner: "role:team" }
  checking: { owner: "field:checker", pass_to: ["role:team"] }
transitions:
  - { action: send, from: [new], to: with_team }
  - { action: check, from: [with_team], to: checking }
  - { action: wait, from: [checking], to: waiting }
  - { action: finish, from: [waiting], to: done }
`

// Made-up people: Asha at the desk, Ben and Dev in the team, Chitra an admin with no other role.
var handMembers = []Member{
	{ID: "asha", Name: "Asha", Roles: []string{"desk"}},
	{ID: "ben", Name: "Ben", Roles: []string{"team"}},
	{ID: "chitra", Name: "Chitra", Roles: []string{"admin"}},
	{ID: "dev", Name: "Dev", Roles: []string{"team"}},
}

func handPlaybook(t *testing.T) *playbook.Playbook {
	t.Helper()
	p, err := playbook.Parse([]byte(handExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func keys(list []Receiver) []string {
	out := make([]string, len(list))
	for i, r := range list {
		out[i] = r.Key()
	}
	return out
}

// TestReceivers: who may receive an item in each step (D68, choice 12): a team step's people and its
// team; any active person elsewhere (people only); never whoever has it now; no team in a step owned
// by a person field; a team nobody is in is marked empty.
func TestReceivers(t *testing.T) {
	p := handPlaybook(t)
	names := map[string]string{"team": "Team"}
	cases := []struct {
		name string
		it   Item
		want []string
	}{
		{"a team step, with the team", Item{State: "with_team", Team: "team"}, []string{"person:ben", "person:dev"}},
		{"a team step, with Ben", Item{State: "with_team", AssigneeID: "ben"}, []string{"person:dev", "team:team"}},
		{"the starter's step", Item{State: "new", AssigneeID: "asha"}, []string{"person:ben", "person:chitra", "person:dev"}},
		{"a step with no owner", Item{State: "waiting", AssigneeID: "dev"}, []string{"person:asha", "person:ben", "person:chitra"}},
		{"a person field's step names no team", Item{State: "checking", AssigneeID: "ben"}, []string{"person:dev"}},
	}
	for _, c := range cases {
		if got := keys(Receivers(p, c.it, handMembers, names)); !slices.Equal(got, c.want) {
			t.Errorf("%s: receivers %v; want %v", c.name, got, c.want)
		}
	}
	list := Receivers(p, Item{State: "with_team", AssigneeID: "ben"}, handMembers, names)
	if team := list[len(list)-1]; team.Name != "the Team team" || team.Empty {
		t.Errorf("the team receiver = %+v; want named, not empty", team)
	}
	list = Receivers(p, Item{State: "with_team", AssigneeID: "ben"}, handMembers[:1], names) // only Asha left
	if len(list) != 1 || !list[0].Empty {
		t.Errorf("with nobody in the team, receivers = %+v; want only the team, marked empty", list)
	}
}

// TestWhoMayPassAndTake: whoever has it (also after losing the team's role) and admins may pass, the
// starter only while nobody has it; people of the team may take it from the team, and anyone the step
// allows may take an item nobody has. An admin sees when nobody can take it (choice 13).
func TestWhoMayPassAndTake(t *testing.T) {
	p := handPlaybook(t)
	actor := func(id string, roles ...string) engine.Actor { return Actor(id, roles) }
	asha, ben, dev, chitra := actor("asha", "desk"), actor("ben", "team"), actor("dev", "team"), actor("chitra", AdminRole)

	withBen := Item{State: "with_team", AssigneeID: "ben", CreatedBy: "asha"}
	for _, c := range []struct {
		name        string
		a           engine.Actor
		ok, asAdmin bool
	}{
		{"Ben, who has it", ben, true, false},
		{"Ben, after losing the team role", actor("ben"), true, false},
		{"Dev, in the team", dev, false, false},
		{"Asha, who started it", asha, false, false},
		{"Chitra, an admin", chitra, true, true},
		{"no person", engine.Actor{}, false, false},
	} {
		if ok, asAdmin := MayPass(withBen, c.a); ok != c.ok || asAdmin != c.asAdmin {
			t.Errorf("%s: MayPass = %v, %v; want %v, %v", c.name, ok, asAdmin, c.ok, c.asAdmin)
		}
	}
	withTeam := Item{State: "with_team", Team: "team", CreatedBy: "asha"}
	if ok, _ := MayPass(withTeam, dev); !ok {
		t.Error("anyone in the team may pass on an item the team has")
	}
	nobody := Item{State: "waiting", CreatedBy: "asha"}
	if ok, _ := MayPass(nobody, asha); !ok {
		t.Error("the starter may pass on an item nobody has")
	}

	if !MayTake(p, withTeam, ben, handMembers) || MayTake(p, withTeam, asha, handMembers) || MayTake(p, withTeam, chitra, handMembers) {
		t.Error("only people in the team may take it from the team (not an admin outside it)")
	}
	if MayTake(p, withBen, dev, handMembers) {
		t.Error("an item Ben has can't be taken")
	}
	if !MayTake(p, Item{State: "waiting"}, asha, handMembers) {
		t.Error("anyone the step allows may take an item nobody has")
	}
	// Work Seyal escalated to the desk team in the team's step (D75, decision 9): someone only at the
	// desk may pass it on, not take it; someone also in the team, or a desk admin, may take it.
	at := "2026-11-06T09:00:00.000000Z"
	escalated := Item{State: "with_team", Team: "desk", CreatedBy: "asha", HeldSince: at, EscalatedAt: at}
	if MayTake(p, escalated, asha, handMembers) {
		t.Error("someone at the desk but not in the team may take escalated team-step work")
	}
	if !MayTake(p, escalated, actor("mira", "desk", "team"), handMembers) || !MayTake(p, escalated, actor("lee", "desk", AdminRole), handMembers) {
		t.Error("someone at the desk who is also in the team, or a desk admin, should be able to take escalated work")
	}
	passed := escalated
	passed.HeldSince = "2026-11-06T10:00:00.000000Z" // passed on after the escalation
	if !MayTake(p, passed, asha, handMembers) {
		t.Error("work passed to the desk team after the escalation may be taken from it as usual")
	}
	// An item with no person and no team is its starter's while they are still a member: nobody else
	// may take it (they ask the starter to pass it on). Once the starter has left, it is free.
	withAsha := Item{State: "waiting", CreatedBy: "asha"}
	if MayTake(p, withAsha, ben, handMembers) || MayTake(p, withAsha, asha, handMembers) || Free(withAsha, handMembers) {
		t.Error("an item with its starter can't be taken")
	}
	if !MayTake(p, withAsha, ben, handMembers[1:]) || !Free(withAsha, handMembers[1:]) {
		t.Error("once the starter has left, anyone the step allows may take it")
	}
	if NobodyCanTake(p, withAsha, handMembers) || NobodyCanTake(p, withAsha, handMembers[1:]) || !NobodyCanTake(p, withAsha, nil) {
		t.Error("an item whose starter left is stuck only when nobody else may take it")
	}

	if NobodyCanTake(p, withTeam, handMembers) || !NobodyCanTake(p, withTeam, handMembers[:1]) {
		t.Error("an item is stuck only when nobody is in its team")
	}
	if NobodyCanTake(p, Item{State: "waiting"}, handMembers) || !NobodyCanTake(p, Item{State: "waiting"}, nil) {
		t.Error("an item nobody has is stuck only when nobody may take it")
	}
	if NobodyCanTake(p, withBen, nil) {
		t.Error("an item a person has isn't stuck")
	}

	for it, want := range map[*Item]string{&withBen: "person:ben", &withTeam: "team:team", &nobody: "nobody"} {
		if got := it.HolderKey(); got != want {
			t.Errorf("HolderKey = %q; want %q", got, want)
		}
	}
	if got := joinNames([]Receiver{{Name: "Ben"}, {Name: "Dev"}, {Name: "the Team team"}}); got != "Ben, Dev, or the Team team" {
		t.Errorf("joinNames = %q", got)
	}
}
