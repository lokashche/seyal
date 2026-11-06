//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// withModelServer switches AI on with a pretend OpenAI-style service on this computer (httptest), so
// the request really goes over HTTP but no real service is called. It answers each request with the
// prepared answer whose key is in the new message (or a note), and returns what it was sent.
func (o *office) withModelServer(answers map[string]string) func() []string {
	o.t.Helper()
	var mu sync.Mutex
	var inputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		input := body.Messages[len(body.Messages)-1].Content
		mu.Lock()
		inputs = append(inputs, input)
		mu.Unlock()
		answer := `{"kind":"note","fields":{},"new_items":[],"command":"","to":"","reason":"","reply":""}`
		msg := input[strings.LastIndex(input, "New message from the person:"):]
		for key, a := range answers {
			if strings.Contains(msg, key) {
				answer = a
			}
		}
		reply, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(reply)
	}))
	o.t.Cleanup(srv.Close)
	settings := map[string]string{"SEYAL_AI_UNDERSTAND": "compat:test-model", "SEYAL_COMPAT_BASE_URL": srv.URL + "/v1"}
	s, err := ai.FromEnv(func(k string) string { return settings[k] })
	if err != nil {
		o.t.Fatal(err)
	}
	c, _ := s.For(ai.Understand)
	o.ai.Set(ai.Understand, c)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), inputs...)
	}
}

// press sends a reply button's words, as the web page does: its text, and which suggestion it answers.
func (o *office) press(item, person string, q ask.Quick) ask.Reply {
	o.t.Helper()
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Converse(o.ctx, tx, o.env(), item, channel.Incoming{Channel: "web", PersonID: person, Text: q.Text, Button: q.Button})
		return err
	})
	return r
}

// pending is the field Seyal's open question is about, or "".
func (tm *team) pending(item string) string {
	tm.t.Helper()
	var key string
	tm.in(func(tx pgx.Tx) error {
		it, err := workitem.Get(tm.ctx, tx, item)
		if err != nil {
			return err
		}
		f, _, err := ask.Pending(tm.ctx, tx, it)
		if f != nil {
			key = f.Key
		}
		return err
	})
	return key
}

// usedBy counts the item's events that name the suggestion as the one confirmed with Yes, by person.
func (tm *team) usedBy(item, person string) int {
	tm.t.Helper()
	return tm.count(`SELECT count(*) FROM ledger WHERE subject_id = $1::uuid AND actor_kind = 'human'
		AND actor_person_id::text = $2 AND body::jsonb->'payload'->'change' ? 'suggestion'`, item, person)
}

// TestSuggestionNeedsYes: the AI model (a pretend service on this computer) reads "Dev-kitta kudu,
// avarukku Sundaram nalla theriyum" as a pass to Dev. Nothing moves before Ben taps Yes; only Ben sees
// the buttons, and Dev's Yes does nothing; on Yes it runs as Ben, linked to the suggestion; the same Yes
// again does nothing; Seyal's open field question stays open (D68, M8; D74).
func TestSuggestionNeedsYes(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.ben, "take it")
	if got := tm.pending(job.ID); got != "required_documents" {
		t.Fatalf("Seyal's open question = %q; want the documents", got)
	}
	inputs := tm.withModelServer(map[string]string{
		"Dev-kitta": `{"kind":"command","fields":{},"new_items":[],"command":"pass","to":"Dev","reason":"he knows Sundaram well","reply":""}`,
		"Bank":      `{"kind":"answer","fields":{"required_documents":"Bank statements"},"new_items":[],"command":"","to":"","reason":"","reply":""}`,
	})

	r := tm.write(job.ID, tm.ben, "Dev-kitta kudu, avarukku Sundaram nalla theriyum")
	if r.Item.AssigneeID != tm.ben || !strings.Contains(said(r), `Pass J-2026-0001 to Dev Example, with the note "he knows Sundaram well"?`) {
		t.Fatalf("the AI's reading: with %q, Seyal said %q; want nothing moved and a suggestion", r.Item.AssigneeID, said(r))
	}
	// Everyone sees the question in the thread, so it names who may answer it, not "you".
	if !strings.Contains(said(r), "Only Ben Example can answer this.") {
		t.Errorf("the suggestion = %q; want it to name Ben as the one who answers", said(r))
	}
	if len(inputs()) != 1 {
		t.Fatalf("%d requests to the model, want 1", len(inputs()))
	}
	q := tm.choosing(job.ID, tm.ben)
	if len(q) != 2 || q[0].Label != "Yes, pass it" || q[0].Text != "yes" || q[1].Label != "No" || !strings.HasPrefix(q[0].Button, "suggestion:") {
		t.Fatalf("Ben's buttons = %+v; want Yes, pass it and No, naming the suggestion", q)
	}
	if other := tm.choosing(job.ID, tm.dev); other != nil {
		t.Errorf("Dev sees Ben's buttons: %+v", other)
	}
	if got := tm.pending(job.ID); got != "required_documents" {
		t.Errorf("after the suggestion, Seyal's open question = %q; want still the documents", got)
	}

	// Dev types yes while Seyal's field question is open: it may be meant for that, so it is read as
	// usual (here the model reads it as a note). Dev presses Ben's Yes from another page: only Ben can
	// confirm it. Dev writing doesn't drop Ben's suggestion.
	if r := tm.write(job.ID, tm.dev, "yes"); r.Item.AssigneeID != tm.ben || strings.Contains(said(r), "Passed to") {
		t.Errorf("Dev's typed yes: with %q, Seyal said %q; want nothing moved", r.Item.AssigneeID, said(r))
	}
	if r := tm.press(job.ID, tm.dev, q[0]); r.Item.AssigneeID != tm.ben || !strings.Contains(said(r), "Only Ben Example can confirm this.") {
		t.Errorf("Dev's Yes button: with %q, Seyal said %q; want nothing moved", r.Item.AssigneeID, said(r))
	}
	if q := tm.choosing(job.ID, tm.ben); len(q) != 2 {
		t.Errorf("after Dev wrote, Ben's buttons = %+v; want still Yes and No", q)
	}

	// Ben taps Yes: it runs as Ben (human), with the note, linked to the suggestion.
	r = tm.press(job.ID, tm.ben, q[0])
	if r.Item.AssigneeID != tm.dev || r.Item.HandoffNote != "he knows Sundaram well" || !strings.Contains(said(r), "Passed to Dev Example.") {
		t.Fatalf("Ben's Yes: with %q note %q, Seyal said %q; want passed to Dev", r.Item.AssigneeID, r.Item.HandoffNote, said(r))
	}
	change, kind, actor := tm.lastAssigned(job.ID)
	if change["how"] != "pass" || change["via"] != "chat" || kind != "human" || actor != tm.ben ||
		fmt.Sprint(change["suggestion"]) != strings.TrimPrefix(q[0].Button, "suggestion:") {
		t.Errorf("the pass event = %v by %s %s; want how pass, via chat, by Ben, naming the suggestion", change, kind, actor)
	}
	if n := tm.usedBy(job.ID, tm.ben); n != 1 {
		t.Errorf("%d events by Ben name a suggestion, want 1", n)
	}
	// The same Yes again (a second tap, or an old page) does nothing.
	if r = tm.press(job.ID, tm.ben, q[0]); r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "Things changed since; say it again.") {
		t.Errorf("a reused Yes: with %q, Seyal said %q; want nothing changed", r.Item.AssigneeID, said(r))
	}
	if n := tm.usedBy(job.ID, tm.ben); n != 1 {
		t.Errorf("after a reused Yes, %d events by Ben name a suggestion, want 1", n)
	}
	// The field question is still open; the new holder answers it.
	if got := tm.pending(job.ID); got != "required_documents" {
		t.Errorf("after the pass, Seyal's open question = %q; want still the documents", got)
	}
	r = tm.write(job.ID, tm.dev, "Bank statements")
	if docs, _ := r.Item.Fields["required_documents"].([]any); len(docs) != 1 {
		t.Errorf("Dev's answer: documents %v; want the open question answered", r.Item.Fields["required_documents"])
	}
	tm.checkLedger()
}

// TestStaleSuggestion: a suggestion stops working when the item changes before Yes, when the person
// writes something else first, or when they say No. A voice note's words become a suggestion too, also
// after "which one?" (D74).
func TestStaleSuggestion(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")

	// Spoken: a suggestion, not a pass. The item then changes hands before Dev taps Yes.
	r := tm.speak(job.ID, tm.dev, "pass to Ben Sample, he is free")
	if r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), `Pass J-2026-0001 to Ben Sample, with the note "he is free"?`) {
		t.Fatalf("a spoken pass: with %q, Seyal said %q; want a suggestion", r.Item.AssigneeID, said(r))
	}
	q := tm.choosing(job.ID, tm.dev)
	if len(q) != 2 {
		t.Fatalf("Dev's buttons = %+v; want Yes and No", q)
	}
	tm.write(job.ID, tm.chitra, "pass to Ben Example") // an admin passes it on meanwhile
	if got := tm.choosing(job.ID, tm.dev); got != nil {
		t.Errorf("the buttons stayed after the item changed: %+v", got)
	}
	if r = tm.press(job.ID, tm.dev, q[0]); r.Item.AssigneeID != tm.ben || !strings.Contains(said(r), "Things changed since; say it again.") {
		t.Errorf("a stale Yes: with %q, Seyal said %q; want nothing moved", r.Item.AssigneeID, said(r))
	}

	// Ben's own next message drops his suggestion; a later "yes" is read as usual.
	r = tm.speak(job.ID, tm.ben, "give it to Dev")
	if !strings.Contains(said(r), "Pass J-2026-0001 to Dev Example?") {
		t.Fatalf("a spoken give: Seyal said %q; want a suggestion", said(r))
	}
	tm.write(job.ID, tm.ben, "the client called about it")
	if got := tm.choosing(job.ID, tm.ben); got != nil {
		t.Errorf("the buttons stayed after Ben wrote again: %+v", got)
	}
	if r = tm.write(job.ID, tm.ben, "yes"); r.Item.AssigneeID != tm.ben || strings.Contains(said(r), "Passed to") {
		t.Errorf("yes after writing again: with %q, Seyal said %q; want nothing moved", r.Item.AssigneeID, said(r))
	}

	// No leaves it as it is.
	tm.speak(job.ID, tm.ben, "give it to Dev")
	if r = tm.write(job.ID, tm.ben, "no"); r.Item.AssigneeID != tm.ben || !strings.Contains(said(r), "Fine, I left it as it is.") {
		t.Errorf("No: with %q, Seyal said %q", r.Item.AssigneeID, said(r))
	}

	// Spoken, with two Bens: "which one?" first; the chosen button then becomes a suggestion (voice).
	tm.speak(job.ID, tm.ben, "pass to ben sample")
	tm.speak(job.ID, tm.ben, "no") // a spoken No
	r = tm.speak(job.ID, tm.ben, "hand to Dev, he knows them")
	q = tm.choosing(job.ID, tm.ben)
	if len(q) != 2 {
		t.Fatalf("Seyal said %q, buttons %+v; want Yes and No", said(r), q)
	}
	if r = tm.press(job.ID, tm.ben, q[0]); r.Item.AssigneeID != tm.dev {
		t.Fatalf("Yes to a spoken pass: with %q, Seyal said %q", r.Item.AssigneeID, said(r))
	}
	if change, kind, actor := tm.lastAssigned(job.ID); change["via"] != "voice" || kind != "human" || actor != tm.ben {
		t.Errorf("the pass event = %v by %s %s; want via voice, by Ben", change, kind, actor)
	}
	r = tm.speak(job.ID, tm.dev, "pass to ben, he knows them")
	if !strings.Contains(said(r), "Which one do you mean") {
		t.Fatalf("a spoken pass to ben: Seyal said %q; want which one", said(r))
	}
	q = tm.choosing(job.ID, tm.dev)
	if r = tm.write(job.ID, tm.dev, q[1].Text); r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "Pass J-2026-0001 to Ben Sample") {
		t.Fatalf("choosing Ben Sample after a spoken pass: with %q, Seyal said %q; want a suggestion", r.Item.AssigneeID, said(r))
	}
	if r = tm.write(job.ID, tm.dev, "yes"); r.Item.AssigneeID != tm.benS {
		t.Errorf("typed yes: with %q, Seyal said %q; want passed to Ben Sample", r.Item.AssigneeID, said(r))
	}
	tm.checkLedger()
}

// TestNotWholeCommandIsASuggestion: "give to Dev the March file when it comes" is not the whole command,
// so nothing moves at once; when the name is one person, Seyal asks first (D74, maintainer's default).
func TestNotWholeCommandIsASuggestion(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.ben, "take it")
	r := tm.write(job.ID, tm.ben, "give to Dev the March file when it comes")
	if r.Item.AssigneeID != tm.ben || !strings.Contains(said(r), "Pass J-2026-0001 to Dev Example?") {
		t.Fatalf("not the whole command: with %q, Seyal said %q; want a suggestion", r.Item.AssigneeID, said(r))
	}
	if r = tm.write(job.ID, tm.ben, "yes"); r.Item.AssigneeID != tm.dev {
		t.Fatalf("yes: with %q, Seyal said %q", r.Item.AssigneeID, said(r))
	}
	if change, _, _ := tm.lastAssigned(job.ID); change["via"] != "chat" || change["suggestion"] == nil {
		t.Errorf("the pass event = %v; want via chat, naming the suggestion", change)
	}
	tm.checkLedger()
}

// chatCheck is a made-up request: Ops takes it, then checks it in a step with no team of its own, where
// work may go to the Sales or the Ops team.
const chatCheck = `id: chat-check
version: "1"
description: A made-up request Ops takes and then checks.
numbering: "K-{seq:4}"
roles: [sales, ops]
fields:
  - { key: topic, label: Topic, type: text, required: true, ask: "What is it about?" }
states: [new, with_ops, checking, done]
initial: new
steps:
  new: { owner: starter, asks: [topic], needs: [topic] }
  with_ops: { owner: "role:ops" }
  checking: { pass_to: ["role:ops", "role:sales"] }
transitions:
  - { action: send, from: [new], to: with_ops, when: required_filled, who: [holder] }
  - { action: check, from: [with_ops], to: checking, who: [holder] }
  - { action: finish, from: [checking], to: done, who: [holder] }
`

// TestGiveBackToTheTeamTakenFrom: in a step with no team of its own, "give back to the team" means the
// team the person took the work from (D74, maintainer's default). Before, Seyal asked which team.
func TestGiveBackToTheTeamTakenFrom(t *testing.T) {
	tm := newTeam(t)
	p, err := playbook.Parse([]byte(chatCheck))
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
	tm.write(item, tm.sam, "send")
	tm.write(item, tm.dev, "take it")
	if r = tm.write(item, tm.dev, "check"); r.Item.State != "checking" || r.Item.AssigneeID != tm.dev {
		t.Fatalf("after check: %s with %q, Seyal said %q", r.Item.State, r.Item.AssigneeID, said(r))
	}
	if r = tm.write(item, tm.dev, "give back to the team"); r.Item.AssigneeID != "" || r.Item.Team != "ops" ||
		!strings.Contains(said(r), "Passed to the Ops team.") {
		t.Errorf("give back to the team: with %q/%q, Seyal said %q; want the Ops team", r.Item.AssigneeID, r.Item.Team, said(r))
	}
	tm.checkLedger()
}

// chatAskWho is a made-up task: Seyal asks who does it, and whoever that names has it.
const chatAskWho = `id: chat-ask-who
version: "1"
description: A made-up task that whoever is named does.
numbering: "W-{seq:4}"
fields:
  - { key: what, label: What needs doing, type: text, required: true, from_message: true }
  - { key: who, label: Who does it, type: person, required: true, ask: "Who does it?" }
states: [open, done]
initial: open
steps:
  open: { owner: "field:who", asks: [who], needs: [what, who] }
transitions:
  - { action: done, from: [open], to: done, when: required_filled }
`

// TestSpokenPersonNeedsYes: a person heard in a voice note, for a field that says who gets the work, is
// only suggested (D68, choice 14; D74). Typed, the same answer is used at once.
func TestSpokenPersonNeedsYes(t *testing.T) {
	o := newOffice(t)
	p, err := playbook.Parse([]byte(chatAskWho))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	o.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(o.ctx, tx, p)
		return err
	})
	start := func() string {
		t.Helper()
		var r ask.Reply
		o.in(func(tx pgx.Tx) error {
			var err error
			r, err = ask.Begin(o.ctx, tx, o.env(), pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: "Water the plants"})
			return err
		})
		o.says(r, "Who does it?")
		return r.Item.ID
	}

	item := start()
	r := o.speak(item, o.sam, "Olga Ops")
	o.says(r, `Set "Who does it" to Olga Ops on W-0001?`)
	if r.Item.Fields["who"] != nil || r.Item.AssigneeID == o.olga {
		t.Fatalf("a spoken name was used before yes: fields %v, with %q", r.Item.Fields, r.Item.AssigneeID)
	}
	if r = o.write(item, o.sam, "yes"); r.Item.Fields["who"] != o.olga || r.Item.AssigneeID != o.olga {
		t.Errorf("after yes: fields %v, with %q; want Olga", r.Item.Fields, r.Item.AssigneeID)
	}

	item = start()
	if r = o.write(item, o.sam, "Olga Ops"); r.Item.Fields["who"] != o.olga {
		t.Errorf("a typed name: fields %v; want it used at once", r.Item.Fields)
	}
	o.checkLedger()
}

// TestAIWhichOneNeedsYes: the AI model reads a hand-over to "Ben", and two people are called Ben. The
// button Dev picks says only which Ben he meant: the pass is then suggested with Yes and No, as for a
// voice note, and nothing moves before Yes (D74).
func TestAIWhichOneNeedsYes(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	tm.withModelServer(map[string]string{
		"Ben-kitta": `{"kind":"command","fields":{},"new_items":[],"command":"pass","to":"Ben","reason":"he is free","reply":""}`,
	})
	r := tm.write(job.ID, tm.dev, "Ben-kitta kudu")
	if r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "Which one do you mean") {
		t.Fatalf("the AI's reading: with %q, Seyal said %q; want which one", r.Item.AssigneeID, said(r))
	}
	q := tm.choosing(job.ID, tm.dev)
	i := slices.IndexFunc(q, func(q ask.Quick) bool { return q.Label == "Ben Sample" })
	if i < 0 {
		t.Fatalf("Dev's buttons = %+v; want one for Ben Sample", q)
	}
	r = tm.press(job.ID, tm.dev, q[i])
	if r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), `Pass J-2026-0001 to Ben Sample, with the note "he is free"?`) {
		t.Fatalf("picking Ben Sample: with %q, Seyal said %q; want a suggestion, nothing moved", r.Item.AssigneeID, said(r))
	}
	q = tm.choosing(job.ID, tm.dev)
	if len(q) != 2 || q[0].Label != "Yes, pass it" {
		t.Fatalf("Dev's buttons = %+v; want Yes and No", q)
	}
	if r = tm.press(job.ID, tm.dev, q[0]); r.Item.AssigneeID != tm.benS {
		t.Fatalf("Yes: with %q, Seyal said %q; want passed to Ben Sample", r.Item.AssigneeID, said(r))
	}
	if change, kind, actor := tm.lastAssigned(job.ID); change["via"] != "chat" || kind != "human" || actor != tm.dev || change["suggestion"] == nil {
		t.Errorf("the pass event = %v by %s %s; want via chat, by Dev, naming the suggestion", change, kind, actor)
	}
	tm.checkLedger()
}

// TestLongModelWordsAreCut: a model answer with a very long "to" or "reason" doesn't put all of it in the
// shared thread. Seyal repeats the words cut short, with "…", like the model's free reply.
func TestLongModelWordsAreCut(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.ben, "take it")
	longTo := strings.Repeat("very big ", 200) + "team"
	longReason := strings.Repeat("he knows Sundaram well ", 100)
	tm.withModelServer(map[string]string{
		"Big-team":  fmt.Sprintf(`{"kind":"command","fields":{},"new_items":[],"command":"pass","to":%q,"reason":"","reply":""}`, longTo),
		"Dev-kitta": fmt.Sprintf(`{"kind":"command","fields":{},"new_items":[],"command":"pass","to":"Dev","reason":%q,"reply":""}`, longReason),
	})

	r := tm.write(job.ID, tm.ben, "Big-team kudu")
	if r.Item.AssigneeID != tm.ben || strings.Contains(said(r), longTo) || len([]rune(said(r))) > 400 {
		t.Errorf("a long \"to\": with %q, Seyal said %d characters: %q; want nothing moved and the words cut", r.Item.AssigneeID, len([]rune(said(r))), said(r))
	}

	r = tm.write(job.ID, tm.ben, "Dev-kitta kudu")
	got := said(r)
	if r.Item.AssigneeID != tm.ben || !strings.Contains(got, "Pass J-2026-0001 to Dev Example, with the note") ||
		strings.Contains(got, strings.TrimSpace(longReason)) || !strings.Contains(got, "…") || len([]rune(got)) > 600 {
		t.Errorf("a long reason: with %q, Seyal said %d characters: %q; want a suggestion with the note cut", r.Item.AssigneeID, len([]rune(got)), got)
	}
	tm.checkLedger()
}

// TestYesOnlyInClearWords: while a suggestion waits, someone else's "no" to Seyal's open question is
// read as usual; a typed "1" answers Seyal's number question and doesn't confirm it; a "yes" in a voice
// note doesn't confirm: Seyal asks again, for a tap or typed words (D74).
func TestYesOnlyInClearWords(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.ben, "take it")

	// Sam's "no" while Seyal asks for the documents: an answer, not "Only Ben … can confirm".
	tm.speak(job.ID, tm.ben, "pass to Dev, he knows them")
	r := tm.write(job.ID, tm.sam, "no")
	if strings.Contains(said(r), "can confirm this") || r.Item.Fields["required_documents"] == nil {
		t.Errorf("Sam's no: Seyal said %q, documents %v; want it read as the answer", said(r), r.Item.Fields["required_documents"])
	}
	if got := tm.pending(job.ID); got != "timeline_days" {
		t.Fatalf("Seyal's open question = %q; want the days", got)
	}

	// Ben's own "1" answers how many days; it doesn't pass the item to Dev.
	tm.speak(job.ID, tm.ben, "pass to Dev, he knows them")
	r = tm.write(job.ID, tm.ben, "1")
	if r.Item.AssigneeID != tm.ben || r.Item.Fields["timeline_days"] != 1.0 || strings.Contains(said(r), "Passed to") {
		t.Errorf("Ben's 1: with %q, days %v, Seyal said %q; want the days answered, nothing moved",
			r.Item.AssigneeID, r.Item.Fields["timeline_days"], said(r))
	}

	// A spoken yes: Seyal asks again; nothing moves before a tap or typed yes.
	tm.speak(job.ID, tm.ben, "pass to Dev, he knows them")
	r = tm.speak(job.ID, tm.ben, "yes")
	if r.Item.AssigneeID != tm.ben || !strings.Contains(said(r), "To say yes to this, tap Yes or type it.") ||
		!strings.Contains(said(r), "Pass J-2026-0001 to Dev Example") {
		t.Fatalf("a spoken yes: with %q, Seyal said %q; want asked again, nothing moved", r.Item.AssigneeID, said(r))
	}
	q := tm.choosing(job.ID, tm.ben)
	if len(q) != 2 {
		t.Fatalf("Ben's buttons = %+v; want Yes and No", q)
	}
	if r = tm.press(job.ID, tm.ben, q[0]); r.Item.AssigneeID != tm.dev {
		t.Fatalf("Yes: with %q, Seyal said %q; want passed to Dev", r.Item.AssigneeID, said(r))
	}
	if change, _, _ := tm.lastAssigned(job.ID); change["via"] != "voice" {
		t.Errorf("the pass event = %v; want via voice", change)
	}
	tm.checkLedger()
}

// chatAskWhoFor is chatAskWho with a list field: who the work is for.
const chatAskWhoFor = `id: chat-ask-who-for
version: "1"
description: A made-up task that whoever is named does, for someone on the list.
numbering: "W-{seq:4}"
fields:
  - { key: what, label: What needs doing, type: text, required: true, from_message: true }
  - { key: for_whom, label: For whom, type: master, kind: client }
  - { key: who, label: Who does it, type: person, required: true, ask: "Who does it?" }
states: [open, done]
initial: open
steps:
  open: { owner: "field:who", asks: [who], needs: [what, who] }
transitions:
  - { action: done, from: [open], to: done, when: required_filled }
`

// TestOfferBeforeSuggestion: the model reads a name that isn't in the list and the person who does the
// work. Seyal asks one yes/no question at a time: the offer to add the name. The person is not suggested
// with it; Seyal asks for them once the offer is answered.
func TestOfferBeforeSuggestion(t *testing.T) {
	o := newOffice(t)
	p, err := playbook.Parse([]byte(chatAskWhoFor))
	if err != nil {
		t.Fatal(err)
	}
	var pv string
	o.in(func(tx pgx.Tx) error {
		pv, _, err = playbook.Register(o.ctx, tx, p)
		return err
	})
	o.withFake(map[string]string{
		"Zeta": `{"kind":"new_work","fields":{"what":"Renew the internet plan","for_whom":"Zeta Traders","who":"Olga Ops"},
			"new_items":[{"field":"for_whom","name":"Zeta Traders"}],"command":"","reason":"","reply":""}`,
	})
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(o.ctx, tx, o.env(), pv, channel.Incoming{Channel: "web", PersonID: o.sam,
			Text: "Olga Ops to renew the internet plan for Zeta Traders"})
		return err
	})
	o.says(r, "Zeta Traders isn't in the list yet.")
	if r.Item.Fields["who"] != nil {
		t.Fatalf("who = %v; want not set before a Yes", r.Item.Fields["who"])
	}
	r = o.write(r.Item.ID, o.sam, "yes")
	o.says(r, "Added Zeta Traders.", "Who does it?")
	if r.Item.Fields["for_whom"] == nil || r.Item.Fields["who"] != nil {
		t.Errorf("after yes: fields %v; want for whom set, who still asked", r.Item.Fields)
	}
	o.checkLedger()
}

// buttons are the reply buttons Seyal shows person under the item's thread.
func (o *office) buttons(item, person string) []ask.Quick {
	o.t.Helper()
	var q []ask.Quick
	o.in(func(tx pgx.Tx) error {
		it, err := workitem.Get(o.ctx, tx, item)
		if err != nil {
			return err
		}
		q, err = ask.Choosing(o.ctx, tx, it, person)
		return err
	})
	return q
}
