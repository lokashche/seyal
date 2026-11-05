//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/playbook"
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
