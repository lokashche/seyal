// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"strings"
	"testing"
	"time"

	"seyal/internal/engine"
	"seyal/internal/playbook"
)

func TestParseQuery(t *testing.T) {
	known := []string{"internal-request", "job-evaluation"}
	tests := []struct {
		name, answer string
		want         Query
		problem      string // part of the error, or "" for none
	}{
		{"open for a name", `{"kind":"query","processes":[],"status":"open","person":"","role":"","entry":"  Acme   Traders ","period":"","count":false}`,
			Query{Kind: "query", Status: "open", Entry: "Acme Traders"}, ""},
		{"no status means open", `{"kind":"query","processes":[],"status":"","person":"","role":"","entry":"","period":"","count":true}`,
			Query{Kind: "query", Status: "open", Count: true}, ""},
		{"a person who finished", `{"kind":"query","processes":[],"status":"finished","person":"Ben","role":"","entry":"","period":"this_week","count":false}`,
			Query{Kind: "query", Status: "finished", Person: "Ben", Role: "finished", Period: "this_week"}, ""},
		{"finished by means finished work", `{"kind":"query","processes":[],"status":"any","person":"Ben","role":"finished","entry":"","period":"","count":false}`,
			Query{Kind: "query", Status: "finished", Person: "Ben", Role: "finished"}, ""},
		{"a person alone has it", `{"kind":"query","processes":[],"status":"open","person":"Ben","role":"","entry":"","period":"","count":false}`,
			Query{Kind: "query", Status: "open", Person: "Ben", Role: "holds"}, ""},
		{"a role alone is mine", `{"kind":"query","processes":[],"status":"open","person":"","role":"approves","entry":"","period":"","count":false}`,
			Query{Kind: "query", Status: "open", Person: "me", Role: "approves"}, ""},
		{"I is me", `{"kind":"query","processes":[],"status":"any","person":"I","role":"started","entry":"","period":"","count":false}`,
			Query{Kind: "query", Status: "any", Person: "me", Role: "started"}, ""},
		{"processes are kept once", "```json\n" + `{"kind":"query","processes":["job-evaluation","job-evaluation"],"status":"overdue","person":"","role":"","entry":"","period":"","count":false}` + "\n```",
			Query{Kind: "query", Processes: []string{"job-evaluation"}, Status: "overdue"}, ""},
		{"a process by its name or in other case", `{"kind":"query","processes":["Internal request","JOB_EVALUATION","job-evaluation"],"status":"open","person":"","role":"","entry":"","period":"","count":false}`,
			Query{Kind: "query", Processes: []string{"internal-request", "job-evaluation"}, Status: "open"}, ""},
		{"cannot drops the rest", `{"kind":"cannot","processes":["nonsense"],"status":"open","person":"","role":"","entry":"","period":"","count":false}`,
			Query{Kind: "cannot"}, ""},
		{"unknown process", `{"kind":"query","processes":["payroll"],"status":"open","person":"","role":"","entry":"","period":"","count":false}`,
			Query{}, "unknown process"},
		{"unknown status", `{"kind":"query","processes":[],"status":"late","person":"","role":"","entry":"","period":"","count":false}`,
			Query{}, "unknown status"},
		{"unknown role", `{"kind":"query","processes":[],"status":"open","person":"Ben","role":"owns","entry":"","period":"","count":false}`,
			Query{}, "unknown role"},
		{"unknown period", `{"kind":"query","processes":[],"status":"open","person":"","role":"","entry":"","period":"next_week","count":false}`,
			Query{}, "unknown period"},
		{"unknown kind", `{"kind":"note"}`, Query{}, "unknown kind"},
		{"not JSON", `I think it's overdue`, Query{}, "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseQuery(tt.answer, known)
			if tt.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tt.problem) {
					t.Fatalf("error = %v, want one about %q", err, tt.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tt.want.Kind || got.Status != tt.want.Status || got.Person != tt.want.Person ||
				got.Role != tt.want.Role || got.Entry != tt.want.Entry || got.Period != tt.want.Period ||
				got.Count != tt.want.Count || strings.Join(got.Processes, ",") != strings.Join(tt.want.Processes, ",") {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestPeriodSpan: periods are worked out in the business's time zone, so the edges are local
// midnights, not UTC ones.
func TestPeriodSpan(t *testing.T) {
	india, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	// Sunday 8 Nov 2026, 23:30 in India: still the week of Monday 2 Nov there (18:00 UTC).
	now := time.Date(2026, 11, 8, 23, 30, 0, 0, india)
	local := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, india) }
	tests := []struct {
		period   string
		from, to time.Time
		words    string
	}{
		{"today", local(2026, 11, 8), local(2026, 11, 9), "today (8 Nov)"},
		{"yesterday", local(2026, 11, 7), local(2026, 11, 8), "yesterday (7 Nov)"},
		{"this_week", local(2026, 11, 2), local(2026, 11, 9), "this week (2 Nov – 8 Nov)"},
		{"last_week", local(2026, 10, 26), local(2026, 11, 2), "last week (26 Oct – 1 Nov)"},
		{"this_month", local(2026, 11, 1), local(2026, 12, 1), "this month (November 2026)"},
		{"last_month", local(2026, 10, 1), local(2026, 11, 1), "last month (October 2026)"},
		{"this_year", local(2026, 1, 1), local(2027, 1, 1), "this year (2026)"},
		{"last_year", local(2025, 1, 1), local(2026, 1, 1), "last year (2025)"},
		{"this_financial_year", local(2026, 4, 1), local(2027, 4, 1), "this financial year (Apr 2026 – Mar 2027)"},
		{"last_financial_year", local(2025, 4, 1), local(2026, 4, 1), "last financial year (Apr 2025 – Mar 2026)"},
		{"last_7_days", local(2026, 11, 2), local(2026, 11, 9), "in the last 7 days (2 Nov – 8 Nov)"},
		{"last_30_days", local(2026, 10, 10), local(2026, 11, 9), "in the last 30 days (10 Oct – 8 Nov)"},
	}
	for _, tt := range tests {
		s, ok := periodSpan(tt.period, now, 4)
		if !ok || !s.From.Equal(tt.from) || !s.To.Equal(tt.to) || s.Words != tt.words {
			t.Errorf("%s = %v to %v %q, want %v to %v %q", tt.period, s.From, s.To, s.Words, tt.from, tt.to, tt.words)
		}
	}
	if _, ok := periodSpan("next_week", now, 4); ok {
		t.Error("an unknown period should not be worked out")
	}

	week, _ := periodSpan("this_week", now, 4)
	// 1 Nov 18:29 UTC is 23:59 on Sunday 1 Nov in India: last week. 18:30 UTC is Monday 00:00 there.
	if week.contains(time.Date(2026, 11, 1, 18, 29, 0, 0, time.UTC)) || !week.contains(time.Date(2026, 11, 1, 18, 30, 0, 0, time.UTC)) {
		t.Error("the week should start at Monday midnight in India, not in UTC")
	}
	// 8 Nov 18:30 UTC is already Monday 9 Nov in India: next week, though it's still Sunday in UTC.
	if week.contains(time.Date(2026, 11, 8, 18, 30, 0, 0, time.UTC)) {
		t.Error("the week should end at Sunday midnight in India")
	}

	// On 31 March the financial year is still the one that started last April; on 1 April a new one.
	if s, _ := periodSpan("this_financial_year", time.Date(2027, 3, 31, 23, 59, 0, 0, india), 4); !s.From.Equal(local(2026, 4, 1)) {
		t.Errorf("31 March: financial year from %v, want 1 Apr 2026", s.From)
	}
	if s, _ := periodSpan("this_financial_year", time.Date(2027, 4, 1, 0, 0, 0, 0, india), 4); !s.From.Equal(local(2027, 4, 1)) {
		t.Errorf("1 April: financial year from %v, want 1 Apr 2027", s.From)
	}
	// A financial year that starts in January is the calendar year.
	if s, _ := periodSpan("this_financial_year", now, 1); !s.From.Equal(local(2026, 1, 1)) || !s.To.Equal(local(2027, 1, 1)) {
		t.Errorf("January start: %v to %v", s.From, s.To)
	}
	// "Last month" in January is December of the year before.
	if s, _ := periodSpan("last_month", time.Date(2027, 1, 15, 10, 0, 0, 0, india), 4); !s.From.Equal(local(2026, 12, 1)) || !s.To.Equal(local(2027, 1, 1)) {
		t.Errorf("last month in January: %v to %v", s.From, s.To)
	}
	// A Monday is the first day of its own week.
	if s, _ := periodSpan("this_week", time.Date(2026, 11, 9, 0, 0, 0, 0, india), 4); !s.From.Equal(local(2026, 11, 9)) {
		t.Errorf("this week on a Monday starts %v", s.From)
	}
}

func TestStage(t *testing.T) {
	p, err := playbook.Parse([]byte(`
id: test-request
version: "1"
description: A test process.
numbering: "T-{seq:4}"
title: "{what}"
roles: [ops, owner]
fields:
  - { key: what, label: What, type: text, required: true }
states: [asking, with_ops, waiting, approved, done, cancelled]
initial: asking
transitions:
  - { action: send, from: [asking], to: with_ops, when: required_filled }
  - { action: back, from: [with_ops], to: asking, needs: reason }
  - { action: ask_owner, from: [with_ops], to: waiting }
  - { action: done, from: [with_ops], to: done }
  - { action: cancel, from: [asking, with_ops, approved], to: cancelled, needs: reason }
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
	for state, want := range map[string]string{
		"asking": "open", "with_ops": "open", // "back" needs a reason but doesn't end the work
		"waiting":   "open", // waiting for approval
		"approved":  "open", // it can still be cancelled, so My Work shows it as open too
		"done":      "finished",
		"cancelled": "cancelled",
	} {
		if got := stage(p, state); got != want {
			t.Errorf("stage(%s) = %s, want %s", state, got, want)
		}
		if open := !engine.IsClosed(p, state); open != (want == "open") { // the same rule as My Work
			t.Errorf("stage(%s) = %s, but engine.IsClosed = %v", state, want, !open)
		}
	}
}

func TestDescribe(t *testing.T) {
	week := &span{Words: "this week (2 Nov – 8 Nov)"}
	tests := []struct {
		q     Query
		n     int
		procs []string
		who   string
		entry string
		when  *span
		want  string
	}{
		{Query{Status: "open"}, 2, nil, "", "Acme Traders", nil, "2 open items for Acme Traders"},
		{Query{Status: "overdue"}, 0, nil, "", "", nil, "No overdue items"},
		{Query{Status: "finished", Role: "finished"}, 1, nil, "Ben Example", "", week, "1 item finished by Ben Example this week (2 Nov – 8 Nov)"},
		{Query{Status: "finished"}, 3, nil, "", "", week, "3 items finished this week (2 Nov – 8 Nov)"},
		{Query{Status: "cancelled"}, 3, nil, "", "", nil, "3 cancelled items"},
		{Query{Status: "open", Role: "approves"}, 1, nil, "you", "", nil, "1 open item waiting for your approval"},
		{Query{Status: "open", Role: "approves"}, 1, nil, "Ben Example", "", nil, "1 open item waiting for Ben Example's approval"},
		{Query{Status: "any", Role: "started"}, 4, []string{"Internal request"}, "you", "", week,
			"4 items of Internal request started by you this week (2 Nov – 8 Nov)"},
		{Query{Status: "open", Role: "holds"}, 2, nil, "you", "", week, "2 open items with you started this week (2 Nov – 8 Nov)"},
	}
	for _, tt := range tests {
		if got := describe(tt.q, tt.n, tt.procs, tt.who, tt.entry, tt.when); got != tt.want {
			t.Errorf("describe(%+v) = %q, want %q", tt.q, got, tt.want)
		}
	}
}

func TestLooksLikeQuestion(t *testing.T) {
	for text, want := range map[string]bool{
		"What is overdue?":                 true,
		"what's pending for Acme Traders?": true,
		"Which items did I start?":         true,
		"Who has the printer request?":     true,
		"How many are open?":               true,
		"What is overdue":                  false, // no question mark: might be the start of a task
		"Can you fix the printer?":         false, // a request
		"The printer is broken?":           false,
		"How do I start work?":             false,
	} {
		if got := looksLikeQuestion(text); got != want {
			t.Errorf("looksLikeQuestion(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestMatchName(t *testing.T) {
	list := []named{{"1", "Ben Example"}, {"2", "Bena Sample"}, {"3", "Ann Example"}, {"4", "Acme Traders"}}
	for said, want := range map[string]string{
		"ben example": "1",   // the same name
		"Ben":         "1",   // every word said is one of the name's words
		"example":     "1,3", // two people: the caller asks which one
		"acme":        "4",
		"trad":        "4", // part of a name
		"Zed":         "",
	} {
		if got := strings.Join(matchName(said, list), ","); got != want {
			t.Errorf("matchName(%q) = %q, want %q", said, got, want)
		}
	}
}

func TestMayDecide(t *testing.T) {
	st := engine.ApprovalStatus{
		Slots: []engine.SlotStatus{{Slot: "ops", Role: "ops", Required: true, ApprovedBy: "p1"},
			{Slot: "owner", Role: "owner", Required: true}},
		Waiting: []string{"owner"},
	}
	for _, tt := range []struct {
		person string
		roles  []string
		want   bool
	}{
		{"p2", []string{"owner"}, true},         // the owner's approval is waiting
		{"p2", []string{"ops"}, false},          // ops has approved already
		{"p1", []string{"ops", "owner"}, false}, // one decision per person per version
		{"p3", nil, false},
	} {
		if got := mayDecide(st, tt.person, tt.roles); got != tt.want {
			t.Errorf("mayDecide(%s, %v) = %v, want %v", tt.person, tt.roles, got, tt.want)
		}
	}
}
