// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notify decides who gets a phone note about a change, and what the note says (D85, Phase 4
// part 4a). A phone note only points at an event that is already in the ledger; it is not a ledger
// event itself (decision 4). This file holds Who; text.go holds the words. Both are pure: they read
// nothing and write nothing, so the reader loop (part 4a-4) and the sender (part 4a-5) can use them
// anywhere, and the tests need no database.
package notify

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// The kinds of phone note (decision 1). Each one is a short fixed sentence (see Text).
const (
	NewWork      = "new_work"      // the item was handed to you
	TeamWork     = "team_work"     // the item was handed to your team
	Approve      = "approve"       // the item waits for your approval
	ApproveAgain = "approve_again" // it changed while it waited, so approvals start again
	Reminder     = "reminder"      // the chaser reminded you
	NoEscalate   = "no_escalate"   // admins only: Seyal wanted to hand it on but nobody could take it
)

// Event is one ledger event about an item, as the reader hands it over: the event's kind, who did it,
// its "change" and its item snapshot.
type Event struct {
	Seq  int64
	Kind string // e.g. "item.assigned"
	// By is the person who acted, or the person Seyal acted for. "" when Seyal acted on its own.
	By string
	// Cause is, for an event Seyal wrote on its own inside a person's change, that person: the first
	// holder of an item Ben starts, the step's owner after Dev's move, work Chitra's refusal sends back.
	// The reader fills it from the other events of the same change (they are written in one
	// transaction). "" when nobody caused it: the chaser, recurring work.
	Cause  string
	Change map[string]any
	Item   workitem.Item // the snapshot the event carries: the item just after the change
}

// Notice is one phone note to make: who gets it, what kind, and what it points at. It holds only ids,
// the item's number and the process's own keys (slots, a team's role), never a title, a hand-over
// note, a field value or a sentence from the thread, so none of those can reach a lock screen.
type Notice struct {
	Person string // who is told
	Kind   string // one of the kinds above
	Item   string // the item's id
	Number string // the item's number, e.g. J-2026-0007: all a phone note says about the item
	Seq    int64  // the ledger event it points at
	From   string // the person who did it; "" when Seyal did it on its own
	Team   string // the team (a role key): TeamWork, and a reminder to a team
	Slots  []string
	// Batch is the same on every move of one admin's bulk move (workitem.HowReassign), so the reader
	// makes one phone note for all of them.
	Batch string
}

// Who returns who gets a phone note about the event, and of what kind. Only the business's active
// people with a login (members, see workitem.Members) are ever told, and never the person who acted
// (By or Cause, decision 6). p is the process version of the event's item. st is the approval status
// of the item's snapshot (engine.Status with the approvals given on that version); only approval
// notes use it.
//
// The events that tell anyone (design section 2.1, "What is recorded"):
//   - item.assigned: the new holder ("New work from Ben"), or everyone in the team that got it. This
//     also covers a new item's first holder: Seyal hands the item over in an item.assigned right
//     after item.started, which itself tells no holder.
//   - item.state_changed into the approval step: everyone who may decide a waiting slot.
//   - item.started of a process whose first step is its approval step: the same. Such an item never
//     writes an item.state_changed into that step; it starts there.
//   - item.fields_set while the item waits for approvals (a new version: approvals start again):
//     everyone who may decide a waiting slot. A copy a step makes when it is entered isn't one: it is
//     part of entering the step, which already told them.
//   - item.posted with chase=remind: whoever the reminder is for.
//   - item.posted with chase=escalate, when nobody could take it: the admins.
//
// item.decided tells nobody (the sender drops waiting approval notes for a slot that is decided).
func Who(e Event, p *playbook.Playbook, members []workitem.Member, st engine.ApprovalStatus) []Notice {
	it := e.Item
	base := Notice{Item: it.ID, Number: it.Number, Seq: e.Seq, From: cmp.Or(e.By, e.Cause)}
	var out []Notice
	tell := func(kind string, m workitem.Member, team string, slots []string) {
		if m.ID == "" || m.ID == e.By || m.ID == e.Cause {
			return
		}
		n := base
		n.Person, n.Kind, n.Team, n.Slots = m.ID, kind, team, slots
		if kind == NewWork {
			n.Batch, _ = e.Change["batch"].(string)
		}
		out = append(out, n)
	}
	// deciders tells everyone who may decide a waiting slot, limited to only when given.
	deciders := func(kind string, only []string) {
		for _, m := range members {
			may := workitem.CanDecide(st, engine.Person{ID: m.ID, Roles: m.Roles})
			var slots []string
			for _, slot := range slices.Sorted(maps.Keys(may)) {
				if only == nil || slices.Contains(only, slot) {
					slots = append(slots, slot)
				}
			}
			if len(slots) > 0 {
				tell(kind, m, "", slots)
			}
		}
	}
	a := p.Approvals

	switch e.Kind {
	case "item.started":
		if a != nil && it.State == a.In {
			deciders(Approve, nil)
		}
	case "item.assigned":
		switch {
		case it.AssigneeID != "":
			if m, ok := member(members, it.AssigneeID); ok {
				tell(NewWork, m, "", nil)
			}
		case it.Team != "":
			for _, m := range members {
				if slices.Contains(m.Roles, it.Team) {
					tell(TeamWork, m, it.Team, nil)
				}
			}
		}
	case "item.state_changed":
		if a != nil && it.State == a.In && e.Change["from"] != a.In {
			deciders(Approve, nil)
		}
	case "item.fields_set":
		if _, copied := e.Change["copied_from"]; a != nil && it.State == a.In && !copied {
			deciders(ApproveAgain, nil)
		}
	case "item.posted":
		switch e.Change["chase"] {
		case "remind":
			to, _ := e.Change["to"].(string)
			kind, name, _ := strings.Cut(to, ":")
			switch kind {
			case "person":
				if m, ok := member(members, name); ok {
					tell(Reminder, m, "", nil)
				}
			case "team":
				for _, m := range members {
					if slices.Contains(m.Roles, name) {
						tell(Reminder, m, name, nil)
					}
				}
			case "approvals":
				deciders(Reminder, strings.Split(name, ","))
			}
			// "nobody": free work that nobody has taken. My Work shows it to whoever may take it.
		case "escalate":
			if workitem.CouldNotEscalate(p, it, members) {
				for _, m := range members {
					if slices.Contains(m.Roles, workitem.AdminRole) {
						tell(NoEscalate, m, "", nil)
					}
				}
			}
		}
	}
	return out
}

// member finds an active person with a login by id.
func member(members []workitem.Member, id string) (workitem.Member, bool) {
	i := slices.IndexFunc(members, func(m workitem.Member) bool { return m.ID == id })
	if i < 0 {
		return workitem.Member{}, false
	}
	return members[i], true
}
