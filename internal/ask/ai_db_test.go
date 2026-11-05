//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/audit"
	"seyal/internal/channel"
	"seyal/internal/ledger"
	"seyal/internal/master"
)

// withFake switches AI on for reading messages, with a fake model that answers each request with
// the first prepared answer whose key appears in the new message.
func (o *office) withFake(answers map[string]string) *ai.Fake {
	f := &ai.Fake{Answer: func(r ai.Request) (string, error) {
		msg := r.Input[strings.LastIndex(r.Input, "New message from the person:"):]
		for key, answer := range answers {
			if strings.Contains(msg, key) {
				if answer == "down" {
					return "", ai.ErrFakeDown
				}
				return answer, nil
			}
		}
		return `{"kind":"note","fields":{},"new_items":[],"command":"","reason":"","reply":""}`, nil
	}}
	o.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: f})
	return f
}

// begin sends a message on Tell Seyal as Sam. A question about the work is then answered in a
// second transaction, as the web app does.
func (o *office) begin(text string) ask.Reply {
	o.t.Helper()
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Begin(o.ctx, tx, o.env(), o.pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: text})
		return err
	})
	if r.Asked != nil {
		o.in(func(tx pgx.Tx) error {
			var err error
			r.Answer, r.Seyal, err = ask.AnswerAsked(o.ctx, tx, o.env(), *r.Asked)
			return err
		})
	}
	return r
}

func (o *office) count(query string, args ...any) int {
	o.t.Helper()
	var n int
	o.in(func(tx pgx.Tx) error { return tx.QueryRow(o.ctx, query, args...).Scan(&n) })
	return n
}

func (o *office) checkLedger() {
	o.t.Helper()
	o.in(func(tx pgx.Tx) error {
		rep, err := ledger.Verify(o.ctx, tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			o.t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(o.ctx, tx)
		if len(problems) > 0 {
			o.t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}

func TestGreetingStartsNothingWithoutAI(t *testing.T) {
	o := newOffice(t)
	r := o.begin("Hi!")
	if r.Item.ID != "" {
		t.Fatalf("a greeting started item %s", r.Item.ID)
	}
	o.says(r, "Tell me about the work you want to start")
	if n := o.count("SELECT count(*) FROM work_items"); n != 0 {
		t.Errorf("%d work items after a greeting, want 0", n)
	}
	o.checkLedger()
}

func TestAIGreetingAndQuestionStartNothing(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Good day": `{"kind":"greeting","fields":{},"new_items":[],"command":"","reason":"","reply":"Good day to you too!"}`,
		"How does": `{"kind":"question","fields":{},"new_items":[],"command":"","reason":"","reply":""}`,
	})
	o.says(o.begin("Good day"), "Good day to you too!")
	o.says(o.begin("How does this work?"), "Tell me about the work you want to start") // no reply given: the standard one
	if n := o.count("SELECT count(*) FROM work_items"); n != 0 {
		t.Errorf("%d work items, want 0", n)
	}
}

// TestAIFillsSeveralDetails: one message gives four details. Each is checked by the rules (the model
// only passes on the words) and saved as "Seyal on behalf of Sam"; Seyal then asks for what's missing.
func TestAIFillsSeveralDetails(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Acme": `{"kind":"new_work","fields":{"client":"Acme","nature_of_work":"monthly","fee_agreed":"12k",
			"integrity_check":"passed"},"new_items":[],"command":"","reason":"","reply":""}`,
	})
	r := o.begin("Acme wants monthly books, agreed 12k, integrity check passed")
	o.says(r, "Why does the client need this?")
	f := r.Item.Fields
	if f["client"] != "acme" || f["nature_of_work"] != "monthly-books" || f["fee_agreed"] != 1200000.0 ||
		f["integrity_check"] != "pass" || f["standard_fee"] != 1200000.0 {
		t.Fatalf("fields = %v", f)
	}
	if n := o.count(`SELECT count(*) FROM ledger WHERE kind = 'item.fields_set' AND actor_kind = 'agent'
		AND on_behalf_of = $1`, o.sam); n != 1 {
		t.Errorf("%d fields_set events by the agent for Sam, want 1", n)
	}
	if n := o.count(`SELECT count(*) FROM ai_calls WHERE ok AND purpose = 'understand' AND person_id = $1`, o.sam); n != 1 {
		t.Errorf("%d recorded AI calls, want 1", n)
	}

	// An answer the model reads as several details again; the next question follows.
	o.withFake(map[string]string{
		"year-end": `{"kind":"answer","fields":{"reason":"year-end books","advance":"5000"},"new_items":[],
			"command":"","reason":"","reply":""}`,
	})
	r = o.write(r.Item.ID, o.sam, "For the year-end books, and they paid 5000 advance")
	if r.Item.Fields["reason"] != "year-end books" || r.Item.Fields["advance"] != 500000.0 {
		t.Fatalf("fields = %v", r.Item.Fields)
	}
	o.says(r, `Everything needed is filled in. Say "submit"`)
	o.checkLedger()
}

// TestAIValuesAreChecked: a value the rules can't use is explained, and the good ones are kept.
func TestAIValuesAreChecked(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Acme": `{"kind":"new_work","fields":{"client":"Acme","fee_agreed":"a lot","standard_fee":"1"},
			"new_items":[],"command":"","reason":"","reply":""}`,
	})
	r := o.begin("Acme, fee a lot")
	o.says(r, "I couldn't use all of that: Fee agreed: I need a number", "What kind of work is it?")
	if r.Item.Fields["client"] != "acme" || r.Item.Fields["fee_agreed"] != nil || r.Item.Fields["standard_fee"] != nil {
		t.Fatalf("fields = %v; want only the client (and never a read-only field)", r.Item.Fields)
	}
}

// TestAIOffersANewMasterItem: a name that isn't in the list is offered as a new entry; "yes" adds it
// (as the person who said yes) and fills the field.
func TestAIOffersANewMasterItem(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Rasi": `{"kind":"new_work","fields":{"client":"Rasi Textiles","nature_of_work":"monthly"},
			"new_items":[{"field":"client","name":"Rasi Textiles"}],"command":"","reason":"","reply":""}`,
	})
	r := o.begin("Monthly books for Rasi Textiles")
	job := r.Item.ID
	o.says(r, "Rasi Textiles isn't in the list yet. Add it as a new client? (yes / no)")
	if r.Item.Fields["nature_of_work"] != "monthly-books" {
		t.Fatalf("fields = %v", r.Item.Fields)
	}

	r = o.write(job, o.sam, "yes")
	o.says(r, "Added Rasi Textiles.", "Why does the client need this?")
	if r.Item.Fields["client"] != "rasi-textiles" {
		t.Fatalf("client = %v", r.Item.Fields["client"])
	}
	o.in(func(tx pgx.Tx) error {
		it, err := master.Get(o.ctx, tx, "client", "rasi-textiles")
		if err == nil && it.Name != "Rasi Textiles" {
			t.Errorf("new client name = %q", it.Name)
		}
		return err
	})
	if n := o.count(`SELECT count(*) FROM ledger WHERE kind = 'master.changed' AND actor_kind = 'human'
		AND actor_person_id = $1 AND body::jsonb->'payload'->>'key' = 'rasi-textiles'`, o.sam); n != 1 {
		t.Errorf("%d master.changed events by Sam, want 1", n)
	}
	o.checkLedger()
}

func TestAIOfferDeclined(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Rasi": `{"kind":"new_work","fields":{"client":"Rasi"},"new_items":[{"field":"client","name":"Rasi"}],
			"command":"","reason":"","reply":""}`,
	})
	r := o.begin("Work for Rasi")
	r = o.write(r.Item.ID, o.sam, "no")
	o.says(r, "Which client is this for?")
	if n := o.count("SELECT count(*) FROM master_items WHERE kind = 'client'"); n != 1 {
		t.Errorf("%d clients, want 1 (nothing added)", n)
	}
}

// TestAIFailureFallsBack: when the provider is down, the rule-based chat carries on, and the failed
// call is recorded.
func TestAIFailureFallsBack(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{"": "down"})
	r := o.begin("New job")
	o.says(r, "Which client is this for?")
	o.says(o.write(r.Item.ID, o.sam, "acme"), "What kind of work is it?")
	if n := o.count(`SELECT count(*) FROM ai_calls WHERE NOT ok AND error LIKE '%down%'`); n != 2 {
		t.Errorf("%d failed AI calls recorded, want 2", n)
	}
	o.checkLedger()
}

func TestAIUnusableAnswerFallsBack(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{"": "I think this is about a client."})
	r := o.begin("New job")
	o.says(r, "Which client is this for?")
	o.says(o.write(r.Item.ID, o.sam, "acme"), "What kind of work is it?")
}

// TestAIReadingNeverApproves: approving needs the person's own words; the model's reading of
// "looks fine to me" is not enough.
func TestAIReadingNeverApproves(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Acme": `{"kind":"new_work","fields":{"client":"Acme","nature_of_work":"monthly","reason":"books",
			"fee_agreed":"12000","integrity_check":"pass"},"new_items":[],"command":"","reason":"","reply":""}`,
		"send it": `{"kind":"command","fields":{},"new_items":[],"command":"submit","reason":"","reply":""}`,
		"fine":    `{"kind":"command","fields":{},"new_items":[],"command":"approve","reason":"","reply":""}`,
		"drop":    `{"kind":"command","fields":{},"new_items":[],"command":"cancel","reason":"not needed","reply":""}`,
	})
	job := o.begin("Acme monthly books, 12000, pass, for books").Item.ID

	o.says(o.write(job, o.sam, "ok send it for approval"), "Noted. Waiting for: sales, ops") // submit needs no reason: allowed
	r := o.write(job, o.olga, "looks fine to me")
	o.says(r, "please type the command itself")
	r = o.write(job, o.olga, "drop this one")
	o.says(r, "please type the command itself")
	if r.Item.State != "pending_approval" {
		t.Fatalf("state = %s", r.Item.State)
	}
	if n := o.count("SELECT count(*) FROM approvals"); n != 0 {
		t.Errorf("%d approvals recorded from the model's reading, want 0", n)
	}
	o.says(o.write(job, o.olga, "approve"), "Noted. Waiting for: sales") // typed by Olga: allowed
}

// TestWhatTheModelSees: the request carries the process and the chosen names, but no person's name
// and no stored field values; authors appear only as "Person" or "Seyal".
func TestWhatTheModelSees(t *testing.T) {
	o := newOffice(t)
	f := o.withFake(map[string]string{
		"Acme": `{"kind":"new_work","fields":{"client":"Acme","nature_of_work":"monthly"},"new_items":[],
			"command":"","reason":"","reply":""}`,
	})
	r := o.begin("Acme monthly books")
	o.write(r.Item.ID, o.sam, "it's for the audit")

	seen := f.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d requests, want 2", len(seen))
	}
	all := seen[0].Instructions + seen[0].Input + seen[1].Instructions + seen[1].Input
	for _, want := range []string{"client", "Acme Traders", "Monthly bookkeeping", "Already filled:", "Seyal: "} {
		if !strings.Contains(all, want) {
			t.Errorf("requests lack %q", want)
		}
	}
	for _, never := range []string{"Chat Test", "Olga", "Mia", "12000", o.sam, o.mia} {
		if strings.Contains(all, never) {
			t.Errorf("requests contain %q:\n%s", never, all)
		}
	}
	if seen[0].Instructions != seen[1].Instructions {
		t.Error("the instructions changed between two messages; they should stay the same so providers can cache them")
	}
}

// TestNewNameAsAnAnswer: without AI, answering "Which client?" with a name that isn't in the list
// offers to add it (tidied: "sheik trading" → "Sheik Trading"); yes adds it and moves on.
func TestNewNameAsAnAnswer(t *testing.T) {
	o := newOffice(t)
	r := o.begin("New job")
	o.says(r, "Which client is this for?")
	o.says(o.write(r.Item.ID, o.sam, "sheik trading"), "Sheik Trading isn't in the list yet. Add it as a new client? (yes / no)")
	r = o.write(r.Item.ID, o.sam, "yes")
	o.says(r, "Added Sheik Trading.", "What kind of work is it?")
	if r.Item.Fields["client"] != "sheik-trading" {
		t.Errorf("client = %v", r.Item.Fields["client"])
	}
	o.checkLedger()
}

// TestAINewNameNotFlagged: the model passes on a new name but forgets to list it in new_items; Seyal
// still offers it instead of saying it doesn't know the name.
func TestAINewNameNotFlagged(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"sheik": `{"kind":"new_work","fields":{"client":"sheik","reason":"registration"},"new_items":[],
			"command":"","reason":"","reply":""}`,
	})
	r := o.begin("sheik wants registration")
	o.says(r, "Sheik isn't in the list yet. Add it as a new client? (yes / no)")
	if r.Item.Fields["reason"] != "registration" {
		t.Errorf("reason = %v, want the other detail kept", r.Item.Fields["reason"])
	}
}

// TestTwoNewNames: one message names a new client and a new work type. Seyal offers them one after
// the other, keeps the details it could use, and only then asks what is still missing.
func TestTwoNewNames(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Rahim": `{"kind":"new_work","fields":{"client":"Rahim Stores","nature_of_work":"GST registration",
			"fee_agreed":"5000"},"new_items":[{"field":"client","name":"Rahim Stores"}],"command":"","reason":"","reply":""}`,
		"Karim": `{"kind":"new_work","fields":{"client":"Karim Traders","nature_of_work":"VAT filing"},"new_items":[],
			"command":"","reason":"","reply":""}`,
	})
	r := o.begin("Rahim Stores wants GST registration for 5000")
	job := r.Item.ID
	o.says(r, "Rahim Stores isn't in the list yet. Add it as a new client? (yes / no)")
	if r.Item.Fields["fee_agreed"] != 500000.0 {
		t.Errorf("fee = %v, want it kept", r.Item.Fields["fee_agreed"])
	}

	r = o.write(job, o.sam, "yes")
	o.says(r, "Added Rahim Stores.", "GST registration isn't in the list yet. Add it as a new work type? (yes / no)")
	r = o.write(job, o.sam, "yes")
	o.says(r, "Added GST registration.", "Why does the client need this?")
	if r.Item.Fields["client"] != "rahim-stores" || r.Item.Fields["nature_of_work"] != "gst-registration" {
		t.Errorf("fields = %v", r.Item.Fields)
	}

	// Saying no to the first still offers the second.
	r = o.begin("Karim Traders wants VAT filing")
	o.says(r, "Karim Traders isn't in the list yet.")
	o.says(o.write(r.Item.ID, o.sam, "no"), "VAT filing isn't in the list yet. Add it as a new work type? (yes / no)")
	o.checkLedger()
}
