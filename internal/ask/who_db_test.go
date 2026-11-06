//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// chatWho is a made-up request: Sales starts it, then Ops has it, and only whoever has it may move it.
const chatWho = `id: chat-who
version: "1"
description: A made-up request Sales starts and Ops finishes.
numbering: "C-{seq:4}"
roles: [sales, ops]
fields:
  - { key: topic, label: Topic, type: text, required: true, ask: "What is it about?" }
  - { key: hours, label: Hours, type: number, required: true, ask: "How many hours?" }
states: [new, with_ops, done, cancelled]
initial: new
steps:
  new: { owner: starter, asks: [topic], needs: [topic] }
  with_ops: { owner: "role:ops", asks: [hours], needs: [hours] }
transitions:
  - { action: send, from: [new], to: with_ops, when: required_filled, who: [holder] }
  - { action: finish, from: [with_ops], to: done, when: required_filled, who: [holder] }
  - { action: cancel, from: [new, with_ops], to: cancelled, needs: reason }
`

// TestChatNamesOnlyMovesYouMayTake: when everything is filled in, Seyal says which move to type only
// if the person may take it (D68). The starter may still fill in the details while Ops has the item,
// but isn't told to type a move only Ops may take.
func TestChatNamesOnlyMovesYouMayTake(t *testing.T) {
	o := newOffice(t)
	p, err := playbook.Parse([]byte(chatWho))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	o.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(o.ctx, tx, p)
		return err
	})
	last := func(r ask.Reply) string {
		t.Helper()
		if len(r.Seyal) == 0 {
			t.Fatal("Seyal said nothing")
		}
		return r.Seyal[len(r.Seyal)-1].Text
	}
	withOps := func() string {
		t.Helper()
		var r ask.Reply
		o.in(func(tx pgx.Tx) error {
			var err error
			r, err = ask.Begin(o.ctx, tx, o.env(), pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: "New request"})
			return err
		})
		item := r.Item.ID
		if got := last(o.write(item, o.sam, "Payroll check")); !strings.Contains(got, `Say "send"`) {
			t.Fatalf("the holder of a ready item was told %q, want to say send", got)
		}
		if r = o.write(item, o.sam, "send"); r.Item.State != "with_ops" {
			t.Fatalf("after send: %s, want with_ops", r.Item.State)
		}
		return item
	}

	item := withOps()
	got := last(o.write(item, o.sam, "6"))
	if strings.Contains(got, `"finish"`) || !strings.Contains(got, "Whoever has it can move it on") {
		t.Errorf("the starter, who doesn't have it, was told %q; want no move named", got)
	}

	item = withOps()
	if got := last(o.write(item, o.olga, "6")); !strings.Contains(got, `Say "finish"`) {
		t.Errorf("Ops, who has it, was told %q; want to say finish", got)
	}
}

// chatTask is a made-up task: whoever "Who does it" names has it.
const chatTask = `id: chat-task
version: "1"
description: A made-up task that whoever is named does.
numbering: "T-{seq:4}"
fields:
  - { key: what, label: What needs doing, type: text, required: true, from_message: true }
  - { key: who, label: Who does it, type: person }
  - { key: due_on, label: Due by, type: date }
states: [open, done]
initial: open
steps:
  open: { owner: "field:who", needs: [what] }
transitions:
  - { action: done, from: [open], to: done, when: required_filled }
`

// TestRefusedWhoKeepsTheRest: naming someone who can't receive the work (here Mia, who has no login)
// is refused, as a pass on would be (D68, choice 14), but the other details read from the same
// message are still saved.
func TestRefusedWhoKeepsTheRest(t *testing.T) {
	o := newOffice(t)
	p, err := playbook.Parse([]byte(chatTask))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	o.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(o.ctx, tx, p)
		return err
	})
	o.withFake(map[string]string{
		"Mia": `{"kind":"new_work","fields":{"who":"Mia Maker","due_on":"2030-01-31"},"new_items":[],"command":"","reason":"","reply":""}`,
	})
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(o.ctx, tx, o.env(), pv, channel.Incoming{Channel: "web", PersonID: o.sam,
			Text: "Ask Mia to water the plants by 31 January 2030"})
		return err
	})
	if r.Item.Fields["due_on"] != "2030-01-31" || r.Item.Fields["who"] != nil {
		t.Errorf("fields = %v; want the due date kept and nobody named", r.Item.Fields)
	}
	var said []string
	for _, m := range r.Seyal {
		said = append(said, m.Text)
	}
	if !strings.Contains(strings.Join(said, " | "), "Mia Maker hasn't joined yet") {
		t.Errorf("Seyal said %q; want it to explain why Mia can't have it", said)
	}
	o.checkLedger()
}

// chatPair is a made-up task: whoever "Approver" names has it now, and whoever "Closer" names gets it
// in the next step (an assign action), so both fields carry power (D68, choice 14).
const chatPair = `id: chat-pair
version: "1"
description: A made-up task one person checks and another closes.
numbering: "P-{seq:4}"
fields:
  - { key: what, label: What needs doing, type: text, required: true, from_message: true }
  - { key: approver, label: Approver, type: person }
  - { key: closer, label: Closer, type: person }
states: [open, closing, done]
initial: open
steps:
  open: { owner: "field:approver", needs: [what] }
transitions:
  - { action: hand_on, from: [open], to: closing }
  - { action: finish, from: [closing], to: done }
on:
  closing:
    - { do: assign, to: closer }
`

// TestOwnerFieldSavedLast: one message names both the step's owner field (Approver) and another power
// field (Closer). The AI read them, so Seyal asks first (D74); on Yes, the owner field sorts first but
// is saved last: saving it hands the item over, and the starter, who no longer had it, could then not
// set Closer (D72 review fix).
func TestOwnerFieldSavedLast(t *testing.T) {
	o := newOffice(t)
	o.login(o.mia) // newOffice gave Sam and Olga theirs
	p, err := playbook.Parse([]byte(chatPair))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	o.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(o.ctx, tx, p)
		return err
	})
	o.withFake(map[string]string{
		"Olga": `{"kind":"new_work","fields":{"approver":"Olga Ops","closer":"Mia Maker"},"new_items":[],"command":"","reason":"","reply":""}`,
	})
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(o.ctx, tx, o.env(), pv, channel.Incoming{Channel: "web", PersonID: o.sam,
			Text: "Olga checks the petty cash, Mia closes it"})
		return err
	})
	if r.Item.Fields["approver"] != nil || r.Item.Fields["closer"] != nil ||
		!strings.Contains(said(r), `Set "Approver" to Olga Ops and "Closer" to Mia Maker on P-0001?`) {
		t.Fatalf("before yes: fields = %v, Seyal said %q; want a suggestion and nothing set", r.Item.Fields, said(r))
	}
	r = o.write(r.Item.ID, o.sam, "yes")
	if r.Item.Fields["approver"] != o.olga || r.Item.Fields["closer"] != o.mia || r.Item.AssigneeID != o.olga {
		var said []string
		for _, m := range r.Seyal {
			said = append(said, m.Text)
		}
		t.Errorf("fields = %v, with %q (Seyal said %q); want both saved and the item with Olga", r.Item.Fields, r.Item.AssigneeID, said)
	}
	o.checkLedger()
}

// chatCloser is a made-up request: Sam starts it, then the Ops team has it. Picking a kind of work
// fills in the Closer, who gets it at the end (an assign action), so the kind of work fills in a field
// that carries power.
const chatCloser = `id: chat-closer
version: "1"
description: A made-up request Sam starts and Ops closes.
numbering: "R-{seq:4}"
roles: [sales, ops]
fields:
  - { key: what, label: What needs doing, type: text, required: true, from_message: true }
  - { key: kind_of_work, label: Kind of work, type: master, kind: work_type, prefill: { closer: maker } }
  - { key: closer, label: Closer, type: person }
  - { key: note, label: Note, type: text }
states: [new, with_ops, closing, done]
initial: new
steps:
  new: { owner: starter }
  with_ops: { owner: "role:ops" }
transitions:
  - { action: send, from: [new], to: with_ops }
  - { action: close, from: [with_ops], to: closing }
  - { action: finish, from: [closing], to: done }
on:
  closing:
    - { do: assign, to: closer }
`

// TestRefusedFillKeepsTheRest: while Ops has the item, Sam names a kind of work that would change the
// Closer, which only whoever has the item may do (D72 review fix). That is refused, but the note from
// the same message is still saved.
func TestRefusedFillKeepsTheRest(t *testing.T) {
	o := newOffice(t)
	p, err := playbook.Parse([]byte(chatCloser))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	o.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(o.ctx, tx, p)
		return err
	})
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(o.ctx, tx, o.env(), pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: "Order new chairs"})
		if err != nil {
			return err
		}
		by := ledger.PersonActor(o.sam)
		if _, err = workitem.SetFields(o.ctx, tx, r.Item.ID, engine.Fields{"closer": o.olga}, by); err != nil {
			return err
		}
		_, err = workitem.Act(o.ctx, tx, r.Item.ID, "send", "", by)
		return err
	})
	o.withFake(map[string]string{
		"audit": `{"kind":"answer","fields":{"kind_of_work":"monthly","note":"for the audit"},"new_items":[],"command":"","reason":"","reply":""}`,
	})
	r = o.write(r.Item.ID, o.sam, "It is monthly work, for the audit")
	var said []string
	for _, m := range r.Seyal {
		said = append(said, m.Text)
	}
	if r.Item.Fields["note"] != "for the audit" || r.Item.Fields["kind_of_work"] != nil || r.Item.Fields["closer"] != o.olga ||
		!strings.Contains(strings.Join(said, " | "), "Closer") {
		t.Errorf("fields = %v (Seyal said %q); want the note saved, the kind of work refused, Olga still the closer", r.Item.Fields, said)
	}
	o.checkLedger()
}
