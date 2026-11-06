//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// team is the chat office with sample job evaluation 0.5 and more people (all made up): Sam (Sales,
// "Test Person") starts jobs; Dev Example, Ben Example and Ben Sample are in Ops with Olga; Chitra
// Example has the owner role and is an admin; Otto Old was in Ops and has left; Mia has no login.
type team struct {
	*office
	pv5                    string // job evaluation 0.5
	dev, ben, benS, chitra string
	otto                   string
}

func newTeam(t *testing.T) *team {
	t.Helper()
	tm := &team{office: newOffice(t)}
	o := tm.office
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	o.in(func(tx pgx.Tx) error {
		if _, err := tx.Exec(o.ctx, "INSERT INTO roles (tenant_id, key, name) VALUES (current_tenant(), 'owner', 'Owner'), (current_tenant(), 'admin', 'Admin')"); err != nil {
			return err
		}
		var err error
		if tm.pv5, _, err = playbook.Register(o.ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		add := func(name, email string, roles ...string) string {
			id, err2 := people.Add(o.ctx, tx, name, email, ledger.SystemActor)
			if err2 != nil {
				err = err2
				return ""
			}
			for _, r := range roles {
				if _, err2 := people.Grant(o.ctx, tx, id, r, ledger.SystemActor); err2 != nil {
					err = err2
				}
			}
			return id
		}
		tm.dev = add("Dev Example", "dev@acme.example", "ops")
		tm.ben = add("Ben Example", "ben@acme.example", "ops")
		tm.benS = add("Ben Sample", "ben.sample@acme.example", "ops")
		tm.chitra = add("Chitra Example", "chitra@acme.example", "owner", "admin")
		tm.otto = add("Otto Old", "otto@acme.example", "ops")
		return err
	})
	o.login(tm.dev, tm.ben, tm.benS, tm.chitra, tm.otto)
	o.in(func(tx pgx.Tx) error {
		_, err := people.SetActive(o.ctx, tx, tm.otto, false, ledger.SystemActor)
		return err
	})
	return tm
}

// withOps starts a job on 0.5 as Sam and sends it to Ops by chat, so Seyal asks its first Ops question.
func (tm *team) withOps() workitem.Item {
	tm.t.Helper()
	var it workitem.Item
	tm.in(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Start(tm.ctx, tx, workitem.NewItem{PlaybookVersionID: tm.pv5, Via: "chat", By: ledger.PersonActor(tm.sam),
			Fields: engine.Fields{"client": "acme", "nature_of_work": "monthly-books", "reason": "Monthly books", "integrity_check": "pass"}})
		return err
	})
	r := tm.write(it.ID, tm.sam, "send to ops")
	if r.Item.State != "with_ops" || r.Item.Team != "ops" {
		tm.t.Fatalf("after send to ops: %s with %q/%q; want with the Ops team", r.Item.State, r.Item.AssigneeID, r.Item.Team)
	}
	return r.Item
}

// said is everything Seyal answered, joined.
func said(r ask.Reply) string {
	var out []string
	for _, m := range r.Seyal {
		out = append(out, m.Text)
	}
	return strings.Join(out, " | ")
}

// lastAssigned is the change and the actor of the item's latest item.assigned event.
func (tm *team) lastAssigned(id string) (change map[string]any, actorKind, actor string) {
	tm.t.Helper()
	tm.in(func(tx pgx.Tx) error {
		var body string
		if err := tx.QueryRow(tm.ctx, `SELECT body FROM ledger WHERE subject_id = $1::uuid AND kind = 'item.assigned'
			ORDER BY seq DESC LIMIT 1`, id).Scan(&body); err != nil {
			return err
		}
		var e struct {
			ActorKind   string `json:"actor_kind"`
			ActorPerson string `json:"actor"`
			Payload     struct {
				Change map[string]any `json:"change"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return err
		}
		change, actorKind, actor = e.Payload.Change, e.ActorKind, e.ActorPerson
		return nil
	})
	return change, actorKind, actor
}

func (tm *team) choosing(item, person string) []ask.Quick {
	tm.t.Helper()
	var q []ask.Quick
	tm.in(func(tx pgx.Tx) error {
		it, err := workitem.Get(tm.ctx, tx, item)
		if err != nil {
			return err
		}
		q, err = ask.Choosing(tm.ctx, tx, it, person)
		return err
	})
	return q
}

// TestTypedTakeAndPass: "I'll take it", "pass to …", "which one?", "give back to the team", through the
// conversation (D68, section 6; D73). Refusals move nothing; the version never changes; `seyal verify`
// stays clean.
func TestTypedTakeAndPass(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	version := job.Version

	r := tm.write(job.ID, tm.dev, "I'll take it")
	if r.Item.AssigneeID != tm.dev || r.Item.Team != "" || !strings.Contains(said(r), "It's yours now.") {
		t.Fatalf("I'll take it: with %q/%q, Seyal said %q; want Dev's", r.Item.AssigneeID, r.Item.Team, said(r))
	}
	if change, kind, actor := tm.lastAssigned(job.ID); change["how"] != "take" || change["via"] != "chat" || kind != "human" || actor != tm.dev {
		t.Errorf("the take event = %v by %s %s; want how take, via chat, by Dev", change, kind, actor)
	}
	if r = tm.write(job.ID, tm.ben, "take it"); r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "Dev Example has it. Ask Dev Example to pass it to you.") {
		t.Errorf("a second take: with %q, Seyal said %q; want it still with Dev", r.Item.AssigneeID, said(r))
	}

	for _, tt := range []struct{ text, want string }{
		{"pass to Test Person", "Test Person can't receive it in this step. It can go to: Ben Example, Ben Sample, Olga Ops, or the Ops team."},
		{"pass to Deb", "I don't know Deb here. It can go to: Ben Example, Ben Sample, Olga Ops, or the Ops team."},
		{"pass to Mia", "Mia Maker hasn't joined yet (no login)"},
		{"pass to otto, he knows them", "Otto Old is no longer active"},
		{"give to Ben the March file when it comes", `If you want to pass it on, type "pass to"`},
		{"pass to Dev", "It's already with Dev Example."},
		{"pass to the sales team", "can't receive it in this step"},
	} {
		r = tm.write(job.ID, tm.dev, tt.text)
		if r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), tt.want) {
			t.Errorf("%q: with %q, Seyal said %q; want it still with Dev and %q", tt.text, r.Item.AssigneeID, said(r), tt.want)
		}
	}

	// Two people are called Ben: Seyal asks which one, with a button each, only for Dev.
	r = tm.write(job.ID, tm.dev, "pass to ben, he knows them")
	if !strings.Contains(said(r), "Which one do you mean: Ben Example, or Ben Sample?") || r.Item.AssigneeID != tm.dev {
		t.Fatalf("pass to ben: Seyal said %q, with %q; want which one", said(r), r.Item.AssigneeID)
	}
	q := tm.choosing(job.ID, tm.dev)
	if len(q) != 2 || q[1].Label != "Ben Sample" || q[1].Text != "pass to Ben Sample, he knows them" {
		t.Fatalf("buttons for Dev = %+v; want one per Ben, sending the whole command", q)
	}
	if other := tm.choosing(job.ID, tm.ben); other != nil {
		t.Errorf("Ben sees Dev's buttons: %+v", other)
	}
	r = tm.write(job.ID, tm.dev, q[1].Text)
	if r.Item.AssigneeID != tm.benS || r.Item.HandoffNote != "he knows them" || !strings.Contains(said(r), "Passed to Ben Sample.") {
		t.Fatalf("pressing Ben Sample: with %q note %q, Seyal said %q", r.Item.AssigneeID, r.Item.HandoffNote, said(r))
	}
	if change, kind, actor := tm.lastAssigned(job.ID); change["how"] != "pass" || change["via"] != "chat" || kind != "human" || actor != tm.dev ||
		change["note"] != "he knows them" {
		t.Errorf("the pass event = %v by %s %s; want how pass, via chat, by Dev, with the note", change, kind, actor)
	}
	if q := tm.choosing(job.ID, tm.dev); q != nil {
		t.Errorf("the buttons stayed after Dev wrote again: %+v", q)
	}

	r = tm.write(job.ID, tm.benS, "give back to the team")
	if r.Item.AssigneeID != "" || r.Item.Team != "ops" || !strings.Contains(said(r), "Passed to the Ops team.") {
		t.Errorf("give back to the team: with %q/%q, Seyal said %q", r.Item.AssigneeID, r.Item.Team, said(r))
	}
	tm.write(job.ID, tm.ben, "take it")
	r = tm.write(job.ID, tm.ben, "pass to the ops team: busy this week")
	if r.Item.Team != "ops" || r.Item.HandoffNote != "busy this week" {
		t.Errorf("pass to the ops team: with %q/%q note %q, Seyal said %q", r.Item.AssigneeID, r.Item.Team, r.Item.HandoffNote, said(r))
	}

	// An admin may pass to someone outside the step's list, and is told so.
	r = tm.write(job.ID, tm.chitra, "assign to Test Person")
	if r.Item.AssigneeID != tm.sam || !strings.Contains(said(r), "Passed to Test Person as an admin.") {
		t.Errorf("an admin's pass: with %q, Seyal said %q", r.Item.AssigneeID, said(r))
	}
	if r.Item.Version != version {
		t.Errorf("hand-overs changed the version from %d to %d", version, r.Item.Version)
	}
	tm.checkLedger()
}

// TestTypedSendBack: "send back to <person|step> because …" finds the move marked back (D68, M4).
func TestTypedSendBack(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")

	for _, tt := range []struct{ text, want string }{
		{"send back to Test Person", `Please add a reason, for example "send back to Test Person because …".`},
		{"send back to Chitra because the fee", "Chitra Example hasn't had this in any step it can go back to. It can go back to: draft (Test Person)."},
		{"send back to Deb because x", "I don't know Deb here. It can go back to: draft (Test Person)."},
		{"send back to studying because x", "It can't go back there from here."},
	} {
		r := tm.write(job.ID, tm.dev, tt.text)
		if r.Item.State != "with_ops" || !strings.Contains(said(r), tt.want) {
			t.Errorf("%q: %s, Seyal said %q; want still with Ops and %q", tt.text, r.Item.State, said(r), tt.want)
		}
	}
	if r := tm.write(job.ID, tm.ben, "send back to Test Person because x"); r.Item.State != "with_ops" ||
		!strings.Contains(said(r), "I couldn't send it back: only whoever has it") {
		t.Errorf("Ben, who doesn't have it: %s, Seyal said %q; want refused", r.Item.State, said(r))
	}

	r := tm.write(job.ID, tm.dev, "send back to Test Person: the March statement is missing")
	if r.Item.State != "draft" || r.Item.AssigneeID != tm.sam || r.Item.HandoffNote != "the March statement is missing" ||
		!strings.Contains(said(r), "Sent back to Test Person (draft).") {
		t.Fatalf("send back to Sam: %s with %q note %q, Seyal said %q", r.Item.State, r.Item.AssigneeID, r.Item.HandoffNote, said(r))
	}
	// Recorded like the move's own word on the panel: the move by Dev, the hand-over after it by Seyal,
	// with no "via" (D73).
	if n := tm.count(`SELECT count(*) FROM ledger WHERE subject_id = $1::uuid AND kind = 'item.state_changed'
		AND body::jsonb->'payload'->'change'->>'action' = 'back_to_sales' AND actor_person_id::text = $2`, job.ID, tm.dev); n != 1 {
		t.Errorf("%d back_to_sales moves by Dev in the ledger, want 1", n)
	}
	if change, kind, _ := tm.lastAssigned(job.ID); change["how"] != "return" || change["via"] != nil || kind != "system" {
		t.Errorf("the hand-over after the send-back = %v by %s; want how return, no via, by Seyal", change, kind)
	}
	// Sam sends it again: it goes straight back to Dev (returns_to: last). Dev sends it back by the
	// step's name, then (sent again) by the move's own word.
	if r = tm.write(job.ID, tm.sam, "send to ops"); r.Item.AssigneeID != tm.dev {
		t.Fatalf("sent again: with %q; want Dev", r.Item.AssigneeID)
	}
	if r = tm.write(job.ID, tm.dev, "send it back to draft because the reason is unclear"); r.Item.State != "draft" || r.Item.AssigneeID != tm.sam {
		t.Errorf("send back to draft: %s with %q, Seyal said %q", r.Item.State, r.Item.AssigneeID, said(r))
	}
	tm.write(job.ID, tm.sam, "send to ops")
	if r = tm.write(job.ID, tm.dev, "send back to sales because one more thing"); r.Item.State != "draft" {
		t.Errorf("send back to sales: %s, Seyal said %q", r.Item.State, said(r))
	}
	tm.checkLedger()
}

// TestHandOverWinsOverAnswer: Seyal's question stays open, but hand-over words win over an answer to it
// (decision 20). The new holder then answers the question. A voice note never hands over (decision 15).
func TestHandOverWinsOverAnswer(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	var asked *playbook.Field
	tm.in(func(tx pgx.Tx) error {
		it, err := workitem.Get(tm.ctx, tx, job.ID)
		if err != nil {
			return err
		}
		asked, _, err = ask.Pending(tm.ctx, tx, it)
		return err
	})
	if asked == nil || asked.Key != "required_documents" {
		t.Fatalf("Seyal's open question = %v; want the documents", asked)
	}

	if r := tm.speak(job.ID, tm.dev, "pass to Ben Sample"); r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "type it or tap the button") {
		t.Errorf("a spoken pass: with %q, Seyal said %q; want nothing moved", r.Item.AssigneeID, said(r))
	}
	r := tm.write(job.ID, tm.dev, "give to Ben Sample")
	if r.Item.AssigneeID != tm.benS || r.Item.Fields["required_documents"] != nil {
		t.Fatalf("give to Ben Sample: with %q, documents %v; want passed, not an answer", r.Item.AssigneeID, r.Item.Fields["required_documents"])
	}
	r = tm.write(job.ID, tm.benS, "Bank statements")
	if docs, _ := r.Item.Fields["required_documents"].([]any); len(docs) != 1 || docs[0] != "Bank statements" {
		t.Errorf("Ben Sample's answer: documents %v; want it to answer the open question", r.Item.Fields["required_documents"])
	}
	tm.checkLedger()
}

// TestModelDoesNotSeeHandOverNames: Seyal's replies that name people ("Passed to Ben Sample.") are left
// out of what the AI model reads (D68, decision 18).
func TestModelDoesNotSeeHandOverNames(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	tm.write(job.ID, tm.dev, "pass to Ben Sample, he knows them")
	f := tm.withFake(map[string]string{})
	tm.write(job.ID, tm.benS, "the client called about it")
	seen := f.Seen()
	if len(seen) != 1 {
		t.Fatalf("%d requests, want 1", len(seen))
	}
	if strings.Contains(seen[0].Input, "Passed to") || strings.Contains(seen[0].Input, "Ben Sample.") {
		t.Errorf("the model saw Seyal's reply naming a person:\n%s", seen[0].Input)
	}
	if !strings.Contains(seen[0].Input, "Person: pass to Ben Sample, he knows them") {
		t.Errorf("the model didn't see the person's own words, as before (D41):\n%s", seen[0].Input)
	}
}

// TestTypedReplyToWhichOne: while "Which one do you mean?" is open, typing a name on a button picks it,
// as pressing it does, and it is never saved as the answer to the question asked before (D73). Pressing
// "Ben", when "Ben" is also Ben Example's and Ben Sample's first name, passes to Ben without asking again.
func TestTypedReplyToWhichOne(t *testing.T) {
	tm := newTeam(t)
	var ben string
	tm.in(func(tx pgx.Tx) error {
		var err error
		if ben, err = people.Add(tm.ctx, tx, "Ben", "ben.only@acme.example", ledger.SystemActor); err != nil {
			return err
		}
		_, err = people.Grant(tm.ctx, tx, ben, "ops", ledger.SystemActor)
		return err
	})
	tm.login(ben)

	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	r := tm.write(job.ID, tm.dev, "pass to ben, he knows them")
	if !strings.Contains(said(r), "Which one do you mean: Ben, Ben Example, or Ben Sample?") {
		t.Fatalf("pass to ben: Seyal said %q; want which one", said(r))
	}
	// Words that pick no button: a note, not the answer to the open question about the documents.
	r = tm.write(job.ID, tm.dev, "Bank statements")
	if r.Item.AssigneeID != tm.dev || r.Item.Fields["required_documents"] != nil {
		t.Errorf("a reply that picks nothing: with %q, documents %v; want nothing moved or saved", r.Item.AssigneeID, r.Item.Fields["required_documents"])
	}

	tm.write(job.ID, tm.dev, "pass to ben, he knows them")
	r = tm.write(job.ID, tm.dev, "Ben Sample")
	if r.Item.AssigneeID != tm.benS || r.Item.HandoffNote != "he knows them" || r.Item.Fields["required_documents"] != nil ||
		!strings.Contains(said(r), "Passed to Ben Sample.") {
		t.Fatalf("typing Ben Sample: with %q note %q, documents %v, Seyal said %q; want passed to Ben Sample",
			r.Item.AssigneeID, r.Item.HandoffNote, r.Item.Fields["required_documents"], said(r))
	}
	if change, kind, actor := tm.lastAssigned(job.ID); change["how"] != "pass" || change["via"] != "chat" || kind != "human" || actor != tm.dev {
		t.Errorf("the pass event = %v by %s %s; want how pass, via chat, by Dev", change, kind, actor)
	}

	tm.write(job.ID, tm.benS, "pass to ben")
	q := tm.choosing(job.ID, tm.benS)
	if len(q) != 2 || q[0].Label != "Ben" || q[1].Label != "Ben Example" {
		t.Fatalf("buttons = %+v; want Ben and Ben Example", q)
	}
	if r = tm.write(job.ID, tm.benS, q[0].Text); r.Item.AssigneeID != ben || !strings.Contains(said(r), "Passed to Ben.") {
		t.Errorf("pressing Ben: with %q, Seyal said %q; want passed to Ben, not asked again", r.Item.AssigneeID, said(r))
	}
	tm.checkLedger()
}

// TestSpokenSentenceIsReadAsUsual: a voice note that only starts like a hand-over ("give it to them by
// Friday, …") is no hand-over, so it gets no voice guard and still answers Seyal's question (decision
// 15). A spoken real hand-over still moves nothing.
func TestSpokenSentenceIsReadAsUsual(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	r := tm.speak(job.ID, tm.dev, "give it to them by Friday, bank statements")
	if strings.Contains(said(r), "type it or tap the button") || r.Item.Fields["required_documents"] == nil {
		t.Errorf("a spoken sentence: Seyal said %q, documents %v; want it read as an answer", said(r), r.Item.Fields["required_documents"])
	}
	if r = tm.speak(job.ID, tm.dev, "pass to Ben Sample"); r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "type it or tap the button") {
		t.Errorf("a spoken pass: with %q, Seyal said %q; want nothing moved", r.Item.AssigneeID, said(r))
	}
}

// TestModelDoesNotSeeButtonNames: a "which one?" button's words name a person from Seyal's list, so they
// are left out of what the model reads, like Seyal's replies; and a reply naming someone who has since
// been renamed is left out too (decision 18).
func TestModelDoesNotSeeButtonNames(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	tm.write(job.ID, tm.dev, "pass to ben, he knows them")
	q := tm.choosing(job.ID, tm.dev)
	if len(q) != 2 {
		t.Fatalf("buttons = %+v; want two", q)
	}
	tm.write(job.ID, tm.dev, q[1].Text)
	tm.in(func(tx pgx.Tx) error {
		_, err := people.Update(tm.ctx, tx, tm.benS, "Bea Sample", "ben.sample@acme.example", ledger.SystemActor)
		return err
	})
	f := tm.withFake(map[string]string{})
	tm.write(job.ID, tm.benS, "the client called about it")
	seen := f.Seen()
	if len(seen) != 1 {
		t.Fatalf("%d requests, want 1", len(seen))
	}
	for _, name := range []string{"Ben Sample", "Ben Example", "Passed to"} {
		if strings.Contains(seen[0].Input, name) {
			t.Errorf("the model saw %q:\n%s", name, seen[0].Input)
		}
	}
	if !strings.Contains(seen[0].Input, "Person: pass to ben, he knows them") {
		t.Errorf("the model didn't see the person's own words (D41):\n%s", seen[0].Input)
	}
}

// chatBack is a made-up request with two moves back from Ops to the same step.
const chatBack = `id: chat-back
version: "1"
description: A made-up request with two ways back to the start.
numbering: "B-{seq:4}"
roles: [sales, ops]
fields:
  - { key: topic, label: Topic, type: text, required: true, ask: "What is it about?" }
states: [new, with_ops, done]
initial: new
steps:
  new: { owner: starter, returns_to: last, asks: [topic], needs: [topic] }
  with_ops: { owner: "role:ops", returns_to: last }
transitions:
  - { action: send, from: [new], to: with_ops, when: required_filled, who: [holder] }
  - { action: back_to_new, from: [with_ops], to: new, needs: reason, back: true, who: [holder] }
  - { action: redo, from: [with_ops], to: new, needs: reason, back: true, who: [holder] }
  - { action: finish, from: [with_ops], to: done, who: [holder] }
`

// TestSendBackWhichOneSameStep: two moves back to the same step get a button each, told apart by the
// move's own word, and pressing one takes that move instead of asking again.
func TestSendBackWhichOneSameStep(t *testing.T) {
	tm := newTeam(t)
	p, err := playbook.Parse([]byte(chatBack))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	tm.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(tm.ctx, tx, p)
		return err
	})
	var r ask.Reply
	tm.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(tm.ctx, tx, tm.env(), pv, channel.Incoming{Channel: "web", PersonID: tm.sam, Text: "New request"})
		return err
	})
	item := r.Item.ID
	tm.write(item, tm.sam, "Payroll check")
	if r = tm.write(item, tm.sam, "send"); r.Item.State != "with_ops" {
		t.Fatalf("after send: %s, Seyal said %q", r.Item.State, said(r))
	}
	tm.write(item, tm.dev, "take it")
	r = tm.write(item, tm.dev, "send back to new because the topic is unclear")
	q := tm.choosing(item, tm.dev)
	if len(q) != 2 || q[0].Label == q[1].Label || q[0].Text == q[1].Text {
		t.Fatalf("Seyal said %q, buttons %+v; want two different buttons", said(r), q)
	}
	if r = tm.write(item, tm.dev, q[1].Text); r.Item.State != "new" || r.Item.AssigneeID != tm.sam ||
		strings.Contains(said(r), "Which one") {
		t.Fatalf("pressing %q: %s with %q, Seyal said %q; want sent back to Sam", q[1].Label, r.Item.State, r.Item.AssigneeID, said(r))
	}
	if n := tm.count(`SELECT count(*) FROM ledger WHERE subject_id = $1::uuid AND kind = 'item.state_changed'
		AND body::jsonb->'payload'->'change'->>'action' = 'redo' AND actor_person_id::text = $2`, item, tm.dev); n != 1 {
		t.Errorf("%d redo moves by Dev in the ledger, want 1", n)
	}
	tm.checkLedger()
}
