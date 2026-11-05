//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/workitem"
)

// query is the model's reading of a question, as JSON.
func query(status, person, role, entry, period string, count bool) string {
	return fmt.Sprintf(`{"kind":"query","processes":[],"status":%q,"person":%q,"role":%q,"entry":%q,"period":%q,"count":%v}`,
		status, person, role, entry, period, count)
}

// withQuestions switches AI on with a fake model: reading a message on Tell Seyal says it's a question;
// turning a question into a search answers with the first prepared query whose key is in the question.
func (o *office) withQuestions(queries map[string]string) *ai.Fake {
	f := &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if strings.Contains(r.Instructions, "small search") {
			for key, q := range queries {
				if strings.Contains(r.Input, key) {
					return q, nil
				}
			}
			return `{"kind":"other","processes":[],"status":"","person":"","role":"","entry":"","period":"","count":false}`, nil
		}
		return `{"kind":"question","fields":{},"new_items":[],"command":"","reason":"","reply":"Happy to help."}`, nil
	}}
	o.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: f})
	return f
}

// start starts an item as person, with the given fields.
func (o *office) start(person string, fields engine.Fields) workitem.Item {
	o.t.Helper()
	var it workitem.Item
	o.in(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Start(o.ctx, tx, workitem.NewItem{PlaybookVersionID: o.pv, Via: "chat", Fields: fields,
			By: ledger.PersonActor(person)})
		return err
	})
	return it
}

func (o *office) asks(question string) *ask.Answer {
	o.t.Helper()
	r := o.begin(question)
	if r.Item.ID != "" {
		o.t.Fatalf("the question %q started item %s", question, r.Item.Number)
	}
	if r.Answer == nil {
		o.t.Fatalf("the question %q got no answer (Seyal said %v)", question, r.Seyal)
	}
	return r.Answer
}

func ids(a *ask.Answer) string {
	var out []string
	for _, f := range a.Items {
		out = append(out, f.ID)
	}
	return strings.Join(out, ",")
}

// TestAskWithoutAI: without AI, a clear question gets a plain "needs AI" and starts nothing; other
// messages start work as before.
func TestAskWithoutAI(t *testing.T) {
	o := newOffice(t)
	if a := o.asks("What is overdue?"); !strings.Contains(a.Line, "needs AI") || a.Question != "What is overdue?" {
		t.Errorf("answer = %+v", a)
	}
	if r := o.begin("Acme Traders needs monthly books"); r.Item.ID == "" {
		t.Error("new work should still start without AI")
	}
	o.checkLedger()
}

// TestAskAnswersFromTheData: the model only turns the question into a search; Seyal runs it and
// answers. The model never sees the items, people or lists, and asking changes nothing.
func TestAskAnswersFromTheData(t *testing.T) {
	o := newOffice(t)
	fake := o.withQuestions(map[string]string{
		"open for Acme":   query("open", "", "", "Acme Traders", "", false),
		"Olga cancel":     query("cancelled", "Olga", "", "", "this_week", false),
		"Mia cancel":      query("cancelled", "Mia", "finished", "", "", false),
		"did I start":     query("any", "me", "started", "", "", true),
		"overdue":         query("overdue", "", "", "", "", false),
		"Zed":             query("open", "Zed", "", "", "", false),
		"Globex":          query("open", "", "", "Globex", "", false),
		"last year":       query("finished", "", "", "", "last_year", false),
		"total":           `{"kind":"cannot","processes":[],"status":"","person":"","role":"","entry":"","period":"","count":false}`,
		"Bad answer":      `{"kind":"query","processes":["payroll"],"status":"open","person":"","role":"","entry":"","period":"","count":false}`,
		"approve as Olga": query("open", "Olga", "approves", "", "", false),
	})
	mine := o.start(o.sam, engine.Fields{"client": "acme"})
	olgas := o.start(o.olga, engine.Fields{"client": "acme"})
	other := o.start(o.sam, nil)
	o.in(func(tx pgx.Tx) error {
		if _, err := workitem.Act(o.ctx, tx, olgas.ID, "cancel", "started twice", ledger.PersonActor(o.olga)); err != nil {
			return err
		}
		_, err := workitem.SetFields(o.ctx, tx, mine.ID, engine.Fields{"client_data_due": "2026-01-01"}, ledger.PersonActor(o.sam))
		return err
	})

	events := o.count("SELECT count(*) FROM ledger")
	calls := o.count("SELECT count(*) FROM ai_calls")
	a := o.asks("What is open for Acme Traders?")
	if a.Line != "1 open item for Acme Traders:" || ids(a) != mine.ID {
		t.Errorf("open for Acme = %q %v", a.Line, a.Items)
	}
	// Asking changes nothing: only the message itself is recorded, as every message is.
	if n := o.count("SELECT count(*) FROM ledger"); n != events+1 {
		t.Errorf("asking added %d ledger events, want 1 (the message)", n-events)
	}
	if n := o.count("SELECT count(*) FROM ledger WHERE seq = (SELECT max(seq) FROM ledger WHERE tenant_id = current_tenant()) AND kind = 'message.received'"); n != 1 {
		t.Error("the last ledger event should be the message received")
	}
	if n := o.count("SELECT count(*) FROM ai_calls"); n != calls+2 {
		t.Errorf("asking made %d AI calls, want 2 (reading the message, then the search)", n-calls)
	}

	a = o.asks("What did Olga cancel this week?")
	if !strings.HasPrefix(a.Line, "1 item cancelled by Olga Ops this week (") || ids(a) != olgas.ID {
		t.Errorf("Olga's cancelled = %q %v", a.Line, a.Items)
	}
	if a := o.asks("What did Mia cancel?"); a.Line != "No items cancelled by Mia Maker." || len(a.Items) != 0 {
		t.Errorf("Mia's cancelled = %q", a.Line)
	}
	if a := o.asks("How many did I start?"); a.Line != "2 items started by you." || len(a.Items) != 0 {
		t.Errorf("count = %q %v", a.Line, a.Items)
	}
	a = o.asks("What is overdue?")
	if a.Line != "1 overdue item:" || ids(a) != mine.ID || !strings.Contains(a.Items[0].Status, "overdue") {
		t.Errorf("overdue = %q %+v", a.Line, a.Items)
	}
	if a.Items[0].Title == "" || a.Items[0].Number != mine.Number {
		t.Errorf("found = %+v", a.Items[0])
	}
	if a := o.asks("What is open for Zed?"); !strings.Contains(a.Line, "couldn't find anyone called “Zed”") {
		t.Errorf("unknown person = %q", a.Line)
	}
	if a := o.asks("What is open for Globex?"); !strings.Contains(a.Line, "couldn't find “Globex”") {
		t.Errorf("unknown entry = %q", a.Line)
	}
	if a := o.asks("What did we finish last year?"); !strings.HasPrefix(a.Line, "No items finished last year (") {
		t.Errorf("last year = %q", a.Line)
	}
	if a := o.asks("What is waiting to approve as Olga?"); a.Line != "No open items waiting for Olga Ops's approval." {
		t.Errorf("approvals = %q", a.Line)
	}
	if a := o.asks("What's the total this year?"); !strings.Contains(a.Line, "can't look that up") {
		t.Errorf("cannot = %q", a.Line)
	}
	// Not about the work, or an answer Seyal can't use: the reply from reading the message, as before.
	o.says(o.begin("Who are you?"), "Happy to help.")
	o.says(o.begin("Bad answer?"), "Happy to help.")

	// What the model saw: only the question, the rules and the processes; never items, people or lists.
	for _, r := range fake.Seen() {
		if !strings.Contains(r.Instructions, "small search") {
			continue
		}
		if !strings.HasPrefix(r.Input, "New message from the person:\n") || strings.Count(r.Input, "\n") != 2 {
			t.Errorf("search input = %q", r.Input)
		}
		for _, secret := range []string{mine.Number, "Olga Ops", "Mia Maker", "Acme Traders", "started twice", "Monthly bookkeeping"} {
			if strings.Contains(r.Instructions, secret) {
				t.Errorf("the search instructions contain %q", secret)
			}
		}
	}
	_ = other
	if n := o.count("SELECT count(*) FROM work_items"); n != 3 {
		t.Errorf("%d work items, want the 3 started by the test", n)
	}
	o.checkLedger()
}

// TestAskSeesOnlyItsBusiness: a question is answered from the business it is asked in only, and a
// person of another business (or one switched off) sees nothing.
func TestAskSeesOnlyItsBusiness(t *testing.T) {
	o, other := newOffice(t), newOffice(t)
	o.withQuestions(map[string]string{"open for Acme": query("open", "", "", "Acme Traders", "", false)})
	mine := o.start(o.sam, engine.Fields{"client": "acme"})
	theirs := other.start(other.sam, engine.Fields{"client": "acme"}) // the other business has its own Acme Traders
	if a := o.asks("What is open for Acme Traders?"); ids(a) != mine.ID {
		t.Errorf("answer = %v, want only %s (not the other business's %s)", a.Items, mine.ID, theirs.ID)
	}

	o.in(func(tx pgx.Tx) error {
		all, err := workitem.All(o.ctx, tx)
		if err != nil {
			return err
		}
		for _, it := range all {
			if it.ID == theirs.ID {
				t.Error("the other business's item is listed")
			}
		}
		if seen, err := workitem.Visible(o.ctx, tx, other.sam, all); err != nil || len(seen) != 0 {
			t.Errorf("a person of another business sees %d items (%v)", len(seen), err)
		}
		if seen, err := workitem.Visible(o.ctx, tx, o.mia, all); err != nil || len(seen) != len(all) {
			t.Errorf("Mia sees %d of %d items (%v)", len(seen), len(all), err)
		}
		if _, err := tx.Exec(o.ctx, "UPDATE persons SET status = 'disabled' WHERE id::text = $1", o.mia); err != nil {
			return err
		}
		if seen, err := workitem.Visible(o.ctx, tx, o.mia, all); err != nil || len(seen) != 0 {
			t.Errorf("a switched-off person sees %d items (%v)", len(seen), err)
		}
		return nil
	})
}

// TestAskAboutApprovals: "waiting for my approval" lists the items a person may decide now, and no
// longer once they have decided. After the last approval the item goes to its maker, who now does the
// work. It can still be cancelled, so it counts as open (pending, and overdue once past its date), not
// finished: open means exactly what My Work shows as open.
func TestAskAboutApprovals(t *testing.T) {
	o := newOffice(t)
	o.withQuestions(map[string]string{
		"approve as Olga":  query("open", "Olga", "approves", "", "", false),
		"my approval":      query("open", "me", "approves", "", "", false),
		"overdue":          query("overdue", "", "", "", "", false),
		"have we finished": query("finished", "", "", "", "", true),
		"was finished":     query("finished", "", "", "", "", false),
		"pending":          query("open", "", "", "", "", false),
		"Mia have":         query("open", "Mia", "holds", "", "", false),
	})
	it := o.start(o.mia, engine.Fields{"client": "acme", "nature_of_work": "monthly-books", "reason": "Year end",
		"fee_agreed": 1200000.0, "integrity_check": "pass", "client_data_due": "2026-01-02"})
	decide := func(person, slot string) {
		o.in(func(tx pgx.Tx) error {
			_, err := workitem.Decide(o.ctx, tx, it.ID, person, slot, "approve", "")
			return err
		})
	}
	o.in(func(tx pgx.Tx) error {
		_, err := workitem.Act(o.ctx, tx, it.ID, "submit", "", ledger.PersonActor(o.mia))
		return err
	})
	if a := o.asks("What is waiting to approve as Olga?"); a.Line != "1 open item waiting for Olga Ops's approval:" || ids(a) != it.ID {
		t.Errorf("Olga's approvals = %q %v", a.Line, a.Items)
	}
	if a := o.asks("What is waiting for my approval?"); a.Line != "1 open item waiting for your approval:" || ids(a) != it.ID {
		t.Errorf("my approvals = %q %v", a.Line, a.Items)
	}
	decide(o.olga, "ops")
	if a := o.asks("What is waiting to approve as Olga?"); a.Line != "No open items waiting for Olga Ops's approval." {
		t.Errorf("after Olga decided = %q", a.Line)
	}
	if a := o.asks("What is waiting for my approval?"); ids(a) != it.ID {
		t.Errorf("Sam still has to decide, but = %q %v", a.Line, a.Items)
	}
	decide(o.sam, "sales")
	a := o.asks("What is overdue?")
	if ids(a) != it.ID || len(a.Items) != 1 || a.Items[0].Status != "approved · overdue" {
		t.Fatalf("an approved item past its date = %q %+v, want it overdue (as on My Work)", a.Line, a.Items)
	}
	if a := o.asks("How many have we finished?"); a.Line != "No finished items." {
		t.Errorf("finished = %q, want none (approved work can still be cancelled)", a.Line)
	}

	// The approved item is now with its maker, who does the work: pending, and not finished.
	o.in(func(tx pgx.Tx) error {
		got, err := workitem.Get(o.ctx, tx, it.ID)
		if err == nil && (got.State != "approved" || got.AssigneeID != o.mia) {
			t.Fatalf("after the last approval: state %q, with %q; want approved, with the maker", got.State, got.AssigneeID)
		}
		return err
	})
	a = o.asks("What is pending?")
	if a.Line != "1 open item:" || ids(a) != it.ID || a.Items[0].With != "Mia Maker" {
		t.Errorf("pending = %q %+v, want the approved item, with Mia Maker", a.Line, a.Items)
	}
	if a := o.asks("What does Mia have?"); ids(a) != it.ID {
		t.Errorf("Mia's work = %q %v, want the approved item she now holds", a.Line, a.Items)
	}
	if a := o.asks("What was finished?"); a.Line != "No finished items." || len(a.Items) != 0 {
		t.Errorf("finished = %q %v, want none", a.Line, a.Items)
	}
	// Open in an answer is exactly My Work's open: the same items as workitem.List, which My Work shows.
	o.in(func(tx pgx.Tx) error {
		open, err := workitem.List(o.ctx, tx)
		if err == nil && (len(open) != 1 || open[0].ID != it.ID) {
			t.Errorf("My Work's open items = %v, want only the approved item", open)
		}
		return err
	})
}

// TestAskAboutSameNames: two people with exactly the same name can't be told apart by name, so the
// answer covers both and says so, instead of quietly picking one.
func TestAskAboutSameNames(t *testing.T) {
	o := newOffice(t)
	o.withQuestions(map[string]string{"did Olga": query("any", "Olga Ops", "started", "", "", false)})
	var twin string
	o.in(func(tx pgx.Tx) error {
		return tx.QueryRow(o.ctx, "INSERT INTO persons (tenant_id, display_name) VALUES (current_tenant(), 'Olga Ops') RETURNING id::text").Scan(&twin)
	})
	first, second := o.start(o.olga, nil), o.start(twin, nil)
	a := o.asks("What did Olga Ops start?")
	if a.Line != "2 items started by Olga Ops (all 2 people with that name):" || len(a.Items) != 2 ||
		!strings.Contains(ids(a), first.ID) || !strings.Contains(ids(a), second.ID) {
		t.Errorf("same names = %q %v", a.Line, a.Items)
	}
}

// TestAskWhenAIFails: when the provider is down, or the search can't be used, a clear question gets a
// plain "couldn't work that out", starts nothing and gets no greeting, whether the person picked a
// process (Begin) or let Seyal decide (Route). The saved message can still be started as work. A
// message that isn't clearly a question gets the first reply, as before.
func TestAskWhenAIFails(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{"": "down"})
	calls := o.count("SELECT count(*) FROM ai_calls")
	a := o.asks("What is overdue?")
	if !strings.Contains(a.Line, "couldn't work that question out") || a.MessageID == "" {
		t.Errorf("AI down = %+v", a)
	}
	if n := o.count("SELECT count(*) FROM ai_calls"); n != calls+1 {
		t.Errorf("AI down: %d calls, want 1 (no search after reading failed)", n-calls)
	}

	// Reading works; the search fails.
	o.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: &ai.Fake{Answer: func(r ai.Request) (string, error) {
		if strings.Contains(r.Instructions, "small search") {
			return "", ai.ErrFakeDown
		}
		return `{"kind":"question","fields":{},"new_items":[],"command":"","reason":"","reply":"Happy to help."}`, nil
	}}})
	if a := o.asks("Which items are open?"); !strings.Contains(a.Line, "couldn't work that question out") {
		t.Errorf("search down = %q", a.Line)
	}
	o.says(o.begin("How does this work?"), "Happy to help.")

	// Several processes, AI down: the same answer, not a greeting; AI off: "needs AI".
	route := func(text string) ask.Routing {
		var rt ask.Routing
		o.in(func(tx pgx.Tx) error {
			var err error
			rt, err = ask.Route(o.ctx, tx, o.env(), []ask.Process{{VersionID: o.pv, ID: "job-evaluation", Name: "Job evaluation"}},
				channel.Incoming{Channel: "web", PersonID: o.sam, Text: text})
			return err
		})
		return rt
	}
	o.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: &ai.Fake{Answer: func(ai.Request) (string, error) {
		return "", ai.ErrFakeDown
	}}})
	rt := route("Who can collect the documents from Acme Traders tomorrow?")
	if rt.Answer == nil || !strings.Contains(rt.Answer.Line, "couldn't work that question out") ||
		rt.Answer.MessageID != rt.Message.ID || len(rt.Seyal) != 0 || rt.Asked != nil {
		t.Errorf("route with AI down = %+v", rt)
	}
	o.ai = &ai.Setup{}
	if rt := route("What is overdue?"); rt.Answer == nil || !strings.Contains(rt.Answer.Line, "needs AI") {
		t.Errorf("route with AI off = %+v", rt)
	}

	// It was new work after all: one tap starts it.
	if n := o.count("SELECT count(*) FROM work_items"); n != 0 {
		t.Fatalf("questions started %d items", n)
	}
	o.in(func(tx pgx.Tx) error {
		r, err := ask.BeginFrom(o.ctx, tx, o.env(), o.pv, rt.Message.ID, o.sam)
		if err == nil && r.Item.ID == "" {
			t.Error("starting the message as work started nothing")
		}
		return err
	})
	o.checkLedger()
}

// TestAskWithLittleTimeLeft: when the web request has too little time left for the search call (the
// first call was slow), Seyal doesn't make it. A clear question gets "couldn't work that out" with the
// offer to start it as work; any other message gets the first reply. With time enough, it searches.
func TestAskWithLittleTimeLeft(t *testing.T) {
	o := newOffice(t)
	o.withQuestions(map[string]string{"open": query("open", "", "", "", "", true)})
	answer := func(text string, left time.Duration) ask.Reply {
		t.Helper()
		var r ask.Reply
		o.in(func(tx pgx.Tx) error {
			var err error
			r, err = ask.Begin(o.ctx, tx, o.env(), o.pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: text})
			return err
		})
		if r.Asked == nil {
			t.Fatalf("%q wasn't left to answer: %+v", text, r)
		}
		ctx, cancel := context.WithTimeout(o.ctx, left)
		defer cancel()
		if err := db.WithTenant(ctx, o.app, o.tenant, func(tx pgx.Tx) error {
			var err error
			r.Answer, r.Seyal, err = ask.AnswerAsked(ctx, tx, o.env(), *r.Asked)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}

	calls := o.count("SELECT count(*) FROM ai_calls")
	r := answer("How many items are open?", 20*time.Second)
	if r.Answer == nil || !strings.Contains(r.Answer.Line, "couldn't work that question out") || r.Answer.MessageID == "" {
		t.Errorf("little time left = %+v", r.Answer)
	}
	if n := o.count("SELECT count(*) FROM ai_calls"); n != calls+1 {
		t.Errorf("little time left: %d AI calls, want 1 (no search)", n-calls)
	}
	o.says(answer("Tell me about the open work", 20*time.Second), "Happy to help.")

	calls = o.count("SELECT count(*) FROM ai_calls")
	if r := answer("How many items are open?", 2*time.Minute); r.Answer == nil || !strings.Contains(r.Answer.Line, "open item") {
		t.Errorf("time enough = %+v", r.Answer)
	}
	if n := o.count("SELECT count(*) FROM ai_calls"); n != calls+2 {
		t.Errorf("time enough: %d AI calls, want 2", n-calls)
	}
	o.checkLedger()
}
