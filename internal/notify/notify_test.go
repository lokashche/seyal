// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// The demo people of the Phase 4 design (Kaveri Associates (demo)), as the business's active members,
// in the order workitem.Members gives them (by name).
const (
	asha   = "p-asha"
	ben    = "p-ben"
	chitra = "p-chitra"
	dev    = "p-dev"
	elan   = "p-elan"
)

var demo = []workitem.Member{
	{ID: asha, Name: "Asha", Roles: []string{"sales"}},
	{ID: ben, Name: "Ben", Roles: []string{"ops"}},
	{ID: chitra, Name: "Chitra", Roles: []string{"admin", "owner"}},
	{ID: dev, Name: "Dev", Roles: []string{"ops"}},
	{ID: elan, Name: "Elan", Roles: []string{"owner"}},
}

var names = map[string]string{asha: "Asha", ben: "Ben", chitra: "Chitra", dev: "Dev", elan: "Elan"}

var teams = map[string]string{"sales": "Sales", "ops": "Operations", "owner": "Owner"}

// Words that must never reach a phone note: the title, the hand-over note, field values and the
// sentences Seyal writes in the thread (design section 2.1, "Never on the lock screen").
var secrets = []string{"Sundaram", "sundaram-textiles", "gst-return", "GST return", "SECRET-REASON",
	"1500000", "1800000", "500000", "Bank statements", "call Ravi", "Still needed", "Fee agreed", "missing"}

func loadJob(t *testing.T) *playbook.Playbook {
	t.Helper()
	p, err := playbook.LoadFile("../../examples/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// job is J-2026-0007 for Sundaram Textiles, as in the design's story: Dev has it, the fee agreed
// (₹15,000) is below the one Sales proposed (₹18,000) and below the standard fee (₹20,000).
func job(state string) workitem.Item {
	return workitem.Item{
		ID: "item-7", Number: "J-2026-0007", Title: "Sundaram Textiles — GST return", State: state,
		AssigneeID: dev, Version: 4, CreatedBy: asha, HandoffFrom: ben,
		HandoffNote: "Bank statements are missing, call Ravi",
		Fields: engine.Fields{
			"client": "sundaram-textiles", "nature_of_work": "gst-return", "reason": "SECRET-REASON",
			"required_documents": []any{"Bank statements", "Sales register"}, "timeline_days": 21.0,
			"maker": dev, "fee_agreed": 1500000.0, "fee_proposed": 1800000.0, "standard_fee": 2000000.0,
			"advance": 500000.0, "integrity_check": "pass",
		},
	}
}

func lookup(kind, key string) (engine.Fields, bool) {
	switch kind + "/" + key {
	case "work_type/gst-return":
		return engine.Fields{"fixed_price": true, "is_new": false}, true
	case "client/sundaram-textiles":
		return engine.Fields{"is_new": false}, true
	}
	return nil, false
}

// status is the approval status of it, with the approvals given so far (slot → person).
func status(t *testing.T, p *playbook.Playbook, it workitem.Item, approved map[string]string) engine.ApprovalStatus {
	t.Helper()
	hash, err := engine.HashFields(it.Fields)
	if err != nil {
		t.Fatal(err)
	}
	var given []engine.Approval
	for slot, person := range approved {
		given = append(given, engine.Approval{Slot: slot, PersonID: person, Decision: engine.Approve,
			ItemVersion: it.Version, FieldsHash: hash})
	}
	st, err := engine.Status(p, it.Engine(), given, lookup)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// say is what each person told would read on the lock screen: "Dev: New work from Ben: J-2026-0007".
func say(notices []Notice) []string {
	var out []string
	for _, n := range notices {
		out = append(out, names[n.Person]+": "+Text(n.Kind, Words{Number: n.Number, From: names[n.From], Team: teams[n.Team]}))
	}
	return out
}

// story is one change of the design's examples: the event, the approvals given before it, and who
// should be told, in plain words.
type story struct {
	name     string
	event    Event
	approved map[string]string
	members  []workitem.Member // nil: the five demo people
	want     []string
}

func stories() []story {
	withOps, pending, approved := job("with_ops"), job("pending_approval"), job("approved")
	toTeam := job("with_ops")
	toTeam.AssigneeID, toTeam.Team = "", "ops"
	taken := job("with_ops")
	taken.AssigneeID = ben
	toOwners := job("with_ops")
	toOwners.AssigneeID, toOwners.Team = "", "owner"
	toAsha := job("draft")
	toAsha.AssigneeID = asha
	started := job("draft")
	started.AssigneeID = ""
	stuck := job("with_ops")
	stuck.HeldSince, stuck.EscalatedAt = "2026-11-10T18:35:00.000000Z", "2026-11-12T18:35:00.000000Z"
	noOwners := []workitem.Member{demo[0], demo[1], {ID: chitra, Name: "Chitra", Roles: []string{"admin"}}, demo[3]}
	reminder := map[string]any{"chase": "remind", "text": "Reminder for Dev: this has been in With ops for 2 working days. Still needed: Fee agreed."}
	with := func(m map[string]any, key string, v any) map[string]any {
		out := map[string]any{key: v}
		for k, x := range m {
			out[k] = x
		}
		return out
	}
	return []story{
		// The design's week (section 2.1, "Example").
		{name: "Fri 16:40 Ben passes it to Dev", event: Event{Seq: 101, Kind: "item.assigned", By: ben, Item: withOps,
			Change: map[string]any{"how": "pass", "assignee_id": dev, "from_assignee": ben, "note": "Bank statements are missing, call Ravi"}},
			want: []string{"Dev: New work from Ben: J-2026-0007"}},
		{name: "Mon 00:05 the chaser reminds Dev", event: Event{Seq: 102, Kind: "item.posted", Item: withOps,
			Change: with(reminder, "to", "person:"+dev)},
			want: []string{"Dev: Reminder: J-2026-0007 is waiting on you"}},
		{name: "Thu 10:12 Dev changes the fee", event: Event{Seq: 103, Kind: "item.fields_set", By: dev, Item: withOps,
			Change: map[string]any{"values": map[string]any{"fee_agreed": 1500000.0}}}},
		{name: "Thu 10:12 Dev submits", event: Event{Seq: 104, Kind: "item.state_changed", By: dev, Item: pending,
			Change: map[string]any{"from": "with_ops", "to": "pending_approval", "action": "submit"}},
			want: []string{"Asha: Your approval is needed: J-2026-0007", "Ben: Your approval is needed: J-2026-0007",
				"Chitra: Your approval is needed: J-2026-0007", "Elan: Your approval is needed: J-2026-0007"}},
		{name: "Thu 10:30 Ben approves Ops", event: Event{Seq: 105, Kind: "item.decided", By: ben, Item: pending,
			Change: map[string]any{"slot": "ops", "decision": "approve"}}, approved: map[string]string{"ops": ben}},
		{name: "Thu 11:05 Asha approves Sales", event: Event{Seq: 106, Kind: "item.decided", By: asha, Item: pending,
			Change: map[string]any{"slot": "sales", "decision": "approve"}}, approved: map[string]string{"ops": ben, "sales": asha}},
		{name: "Thu 14:02 Chitra approves Owner", event: Event{Seq: 107, Kind: "item.decided", By: chitra, Item: pending,
			Change: map[string]any{"slot": "owner", "decision": "approve"}},
			approved: map[string]string{"ops": ben, "sales": asha, "owner": chitra}},
		{name: "Thu 14:02 Seyal moves it to approved", event: Event{Seq: 108, Kind: "item.state_changed", Cause: chitra, Item: approved,
			Change: map[string]any{"from": "pending_approval", "to": "approved"}}},

		// The other hand-overs.
		{name: "Asha sends it to Operations: the team is told", event: Event{Seq: 201, Kind: "item.assigned", Cause: asha, Item: toTeam,
			Change: map[string]any{"how": "step", "assignee_id": "", "team": "ops", "from_assignee": asha}},
			want: []string{"Ben: New work for Operations from Asha: J-2026-0007", "Dev: New work for Operations from Asha: J-2026-0007"}},
		{name: "Ben passes it to his own team: Dev only", event: Event{Seq: 202, Kind: "item.assigned", By: ben, Item: toTeam,
			Change: map[string]any{"how": "pass", "assignee_id": "", "team": "ops", "from_assignee": ben}},
			want: []string{"Dev: New work for Operations from Ben: J-2026-0007"}},
		{name: "Ben takes it from the team: nobody", event: Event{Seq: 203, Kind: "item.assigned", By: ben, Item: taken,
			Change: map[string]any{"how": "take", "assignee_id": ben, "from_team": "ops"}}},
		{name: "Seyal passes it on for Ben (agent): Ben isn't told", event: Event{Seq: 204, Kind: "item.assigned", By: ben, Item: withOps,
			Change: map[string]any{"how": "pass", "assignee_id": dev, "from_assignee": ben, "via": "chat"}},
			want: []string{"Dev: New work from Ben: J-2026-0007"}},
		{name: "Dev sends it back to Sales", event: Event{Seq: 205, Kind: "item.assigned", Cause: dev, Item: toAsha,
			Change: map[string]any{"how": "return", "assignee_id": asha, "from_assignee": dev, "note": "SECRET-REASON"}},
			want: []string{"Asha: New work from Dev: J-2026-0007"}},
		{name: "Seyal escalates it to the Owner team", event: Event{Seq: 206, Kind: "item.assigned", Item: toOwners,
			Change: map[string]any{"how": "escalate", "assignee_id": "", "team": "owner", "chase": "escalate", "from_holder": dev}},
			want: []string{"Chitra: New work for Owner from Seyal: J-2026-0007", "Elan: New work for Owner from Seyal: J-2026-0007"}},
		{name: "Chitra moves Ben's open work to Dev (one press)", event: Event{Seq: 207, Kind: "item.assigned", By: chitra, Item: withOps,
			Change: map[string]any{"how": "reassign", "assignee_id": dev, "from_assignee": chitra, "from_holder": ben, "batch": "b-1"}},
			want: []string{"Dev: New work from Chitra: J-2026-0007"}},
		{name: "Passed to someone switched off: nobody", event: Event{Seq: 208, Kind: "item.assigned", By: ben, Item: withOps,
			Change: map[string]any{"how": "pass", "assignee_id": dev}}, members: []workitem.Member{demo[0], demo[1], demo[2], demo[4]}},

		// A new item's first holder.
		{name: "Asha starts it: item.started tells nobody", event: Event{Seq: 301, Kind: "item.started", By: asha, Item: started,
			Change: map[string]any{"via": "chat"}}},
		{name: "Asha starts it: Seyal gives it to her (the draft's owner): nobody", event: Event{Seq: 302, Kind: "item.assigned", Cause: asha, Item: toAsha,
			Change: map[string]any{"how": "step", "assignee_id": asha}}},
		{name: "Recurring work starts it for Dev: from Seyal", event: Event{Seq: 303, Kind: "item.assigned", Item: withOps,
			Change: map[string]any{"how": "step", "assignee_id": dev}},
			want: []string{"Dev: New work from Seyal: J-2026-0007"}},

		// Approvals again, and reminders to approvers.
		{name: "Asha changes a value while approvals wait", event: Event{Seq: 401, Kind: "item.fields_set", By: asha, Item: pending,
			Change: map[string]any{"values": map[string]any{"advance": 500000.0}}},
			want: []string{"Ben: Please approve again (it changed): J-2026-0007", "Chitra: Please approve again (it changed): J-2026-0007",
				"Dev: Please approve again (it changed): J-2026-0007", "Elan: Please approve again (it changed): J-2026-0007"}},
		{name: "A copy made when the approval step is entered: nobody", event: Event{Seq: 402, Kind: "item.fields_set", Cause: dev, Item: pending,
			Change: map[string]any{"values": map[string]any{"fee_proposed": 1800000.0}, "copied_from": "fee_agreed"}}},
		{name: "A change after approval brings it back to the approvers", event: Event{Seq: 403, Kind: "item.fields_set", By: dev, Item: pending,
			Change: map[string]any{"values": map[string]any{"advance": 500000.0}, "state_from": "approved"}},
			want: []string{"Asha: Please approve again (it changed): J-2026-0007", "Ben: Please approve again (it changed): J-2026-0007",
				"Chitra: Please approve again (it changed): J-2026-0007", "Elan: Please approve again (it changed): J-2026-0007"}},
		{name: "The chaser reminds the approvers still missing", event: Event{Seq: 404, Kind: "item.posted", Item: pending,
			Change: with(reminder, "to", "approvals:sales,owner")}, approved: map[string]string{"ops": ben},
			want: []string{"Asha: Reminder: J-2026-0007 is waiting on you", "Chitra: Reminder: J-2026-0007 is waiting on you",
				"Elan: Reminder: J-2026-0007 is waiting on you"}},
		{name: "The chaser reminds the team", event: Event{Seq: 405, Kind: "item.posted", Item: toTeam,
			Change: with(reminder, "to", "team:ops")},
			want: []string{"Ben: Reminder: J-2026-0007 is waiting on you", "Dev: Reminder: J-2026-0007 is waiting on you"}},
		{name: "The chaser reminds nobody (free work): nobody", event: Event{Seq: 406, Kind: "item.posted", Item: started,
			Change: with(reminder, "to", "nobody")}},

		// Escalations that moved nothing.
		{name: "Nobody in Owner can take it: the admins are told", event: Event{Seq: 501, Kind: "item.posted", Item: stuck,
			Change: map[string]any{"chase": "escalate", "text": "Couldn't escalate: nobody in Owner can take work. It stays with Dev."}},
			members: noOwners, want: []string{"Chitra: Seyal couldn't escalate J-2026-0007"}},
		{name: "Already with the Owner team: nobody", event: Event{Seq: 502, Kind: "item.posted", Item: func() workitem.Item {
			it := toOwners
			it.HeldSince, it.EscalatedAt = stuck.HeldSince, stuck.EscalatedAt
			return it
		}(), Change: map[string]any{"chase": "escalate", "text": "Still waiting after 4 working days. It is already with Owner."}}},

		// Events that never tell anyone.
		{name: "A plain thread line: nobody", event: Event{Seq: 601, Kind: "item.posted", Item: withOps,
			Change: map[string]any{"text": "Over to Operations. Still needed: Fee agreed."}}},
		{name: "Linked work: nobody", event: Event{Seq: 602, Kind: "item.linked", By: asha, Item: withOps}},
	}
}

func TestWhoIsTold(t *testing.T) {
	p := loadJob(t)
	for _, s := range stories() {
		t.Run(s.name, func(t *testing.T) {
			members := cmpOr(s.members, demo)
			notices := Who(s.event, p, members, status(t, p, s.event.Item, s.approved))
			got := say(notices)
			for _, line := range got {
				t.Log(line)
			}
			if !slices.Equal(got, s.want) {
				t.Errorf("told:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(s.want, "\n  "))
			}
		})
	}
}

func cmpOr(a, b []workitem.Member) []workitem.Member {
	if a != nil {
		return a
	}
	return b
}

// Decision 6: the person who acted is never told, even when they could approve themselves (Dev's Ops
// role lets him approve the job he submitted).
func TestTheActorIsNeverTold(t *testing.T) {
	p := loadJob(t)
	for _, s := range stories() {
		for _, n := range Who(s.event, p, cmpOr(s.members, demo), status(t, p, s.event.Item, s.approved)) {
			if n.Person == s.event.By || n.Person == s.event.Cause {
				t.Errorf("%s: %s, who did it, is told", s.name, names[n.Person])
			}
		}
	}
	// Dev may decide the Ops slot of the job he submitted...
	pending := job("pending_approval")
	st := status(t, p, pending, nil)
	if !workitem.CanDecide(st, engine.Person{ID: dev, Roles: []string{"ops"}})["ops"] {
		t.Fatal("Dev can't decide the Ops slot; the test no longer shows decision 6")
	}
	// ...and still gets no phone note about his own submit.
	e := Event{Seq: 1, Kind: "item.state_changed", By: dev, Item: pending, Change: map[string]any{"from": "with_ops", "to": "pending_approval"}}
	for _, n := range Who(e, p, demo, st) {
		if n.Person == dev {
			t.Error("Dev is told about his own submit")
		}
	}
}

// What each notice says about slots, teams and bulk moves, for the reader (4a-4) and the sender
// (4a-5): which slots a note is for (so a decided slot drops it), and the batch of one bulk move.
func TestNoticeDetails(t *testing.T) {
	p := loadJob(t)
	pending := job("pending_approval")
	e := Event{Seq: 7, Kind: "item.state_changed", By: dev, Item: pending, Change: map[string]any{"from": "with_ops", "to": "pending_approval"}}
	got := map[string]string{}
	for _, n := range Who(e, p, demo, status(t, p, pending, nil)) {
		if n.Item != "item-7" || n.Seq != 7 || n.From != dev || n.Kind != Approve {
			t.Errorf("notice %+v doesn't point at the event", n)
		}
		got[names[n.Person]] = strings.Join(n.Slots, ",")
	}
	want := map[string]string{"Asha": "sales", "Ben": "ops", "Chitra": "owner", "Elan": "owner"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("slots %v, want %v", got, want)
	}

	moved := Event{Seq: 8, Kind: "item.assigned", By: chitra, Item: job("with_ops"),
		Change: map[string]any{"how": "reassign", "assignee_id": dev, "batch": "b-1"}}
	if n := Who(moved, p, demo, engine.ApprovalStatus{}); len(n) != 1 || n[0].Batch != "b-1" || n[0].Team != "" {
		t.Errorf("bulk move: %+v, want one notice with batch b-1", n)
	}
	team := job("with_ops")
	team.AssigneeID, team.Team = "", "ops"
	toTeam := Event{Seq: 9, Kind: "item.assigned", By: asha, Item: team, Change: map[string]any{"how": "pass", "team": "ops"}}
	for _, n := range Who(toTeam, p, demo, engine.ApprovalStatus{}) {
		if n.Kind != TeamWork || n.Team != "ops" || n.Batch != "" {
			t.Errorf("team hand-over: %+v", n)
		}
	}
}

// No title, hand-over note, field value or thread sentence ever reaches a notice or its text.
func TestNothingPrivateIsSaid(t *testing.T) {
	p := loadJob(t)
	for _, s := range stories() {
		notices := Who(s.event, p, cmpOr(s.members, demo), status(t, p, s.event.Item, s.approved))
		all := fmt.Sprintf("%#v", notices)
		for _, n := range notices {
			w := Words{Business: "Kaveri Associates (demo)", Number: n.Number, From: names[n.From], Team: teams[n.Team]}
			all += "\n" + Text(n.Kind, w) + "\n" + Quiet(map[string]int{n.Kind: 2}, w.Business) +
				"\n" + Several(3, w.Business) + "\n" + Moved(12, w.From, w.Business)
		}
		for _, secret := range secrets {
			if strings.Contains(strings.ToLower(all), strings.ToLower(secret)) {
				t.Errorf("%s: %q reaches a phone note:\n%s", s.name, secret, all)
			}
		}
	}
}

// A process may start in its approval step (initial is approvals.in). Such an item never writes an
// item.state_changed into that step, so its item.started must tell the approvers (review fix of 4a-3).
func TestStartsInItsApprovalStep(t *testing.T) {
	p, err := playbook.Parse([]byte(`
id: test-ask-first
version: "1"
description: A test process that waits for approval from the start.
numbering: "T-{seq:4}"
title: "{what}"
roles: [sales, ops, owner]
fields:
  - { key: what, label: What, type: text, required: true }
states: [waiting, approved, asking, cancelled]
initial: waiting
transitions:
  - { action: cancel, from: [waiting, asking, approved], to: cancelled, needs: reason }
approvals:
  in: waiting
  on_complete: approved
  on_reject: asking
  slots:
    - { slot: owner, role: owner }
`))
	if err != nil {
		t.Fatal(err)
	}
	it := workitem.Item{ID: "item-1", Number: "T-0001", State: "waiting", Version: 1,
		Fields: engine.Fields{"what": "Acme Traders laptop"}}
	for _, s := range []struct {
		name string
		by   string
		want []string
	}{
		{"Asha starts it", asha, []string{"Chitra: Your approval is needed: T-0001", "Elan: Your approval is needed: T-0001"}},
		{"Recurring work starts it", "", []string{"Chitra: Your approval is needed: T-0001", "Elan: Your approval is needed: T-0001"}},
		{"Elan starts it: only Chitra", elan, []string{"Chitra: Your approval is needed: T-0001"}},
	} {
		e := Event{Seq: 1, Kind: "item.started", By: s.by, Item: it, Change: map[string]any{"via": "chat"}}
		if got := say(Who(e, p, demo, status(t, p, it, nil))); !slices.Equal(got, s.want) {
			t.Errorf("%s: told %v, want %v", s.name, got, s.want)
		}
	}
}
