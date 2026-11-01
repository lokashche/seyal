// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"seyal/internal/playbook"
)

// fakeChoices is a small, fictional set of people and master items.
type fakeChoices struct{}

func (fakeChoices) People() ([]Choice, error) {
	return []Choice{{"p-mia", "Mia Maker"}, {"p-olga", "Olga Ops"}, {"p-omar", "Omar Owner"}}, nil
}

func (fakeChoices) Items(kind string) ([]Choice, error) {
	if kind != "client" {
		return nil, nil
	}
	return []Choice{{"acme", "Acme Traders"}, {"acme-exports", "Acme Exports"}, {"bolt", "Bolt Garages"}}, nil
}

// Saturday, 31 October 2026.
var today = time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)

func TestUnderstand(t *testing.T) {
	field := func(typ string) playbook.Field { return playbook.Field{Key: "x", Type: typ} }
	choice := playbook.Field{Key: "x", Type: "choice", Options: []string{"pass", "fail"}}
	client := playbook.Field{Key: "x", Type: "master", Kind: "client"}
	people := playbook.Field{Key: "x", Type: "list", Of: "person"}

	tests := []struct {
		field   playbook.Field
		answer  string
		want    any
		wantErr string
	}{
		// money is stored in minor units
		{field("money"), "12000", 1200000.0, ""},
		{field("money"), "12,000", 1200000.0, ""},
		{field("money"), "₹12k", 1200000.0, ""},
		{field("money"), "Rs. 1.5 lakh", 15000000.0, ""},
		{field("money"), "12000/-", 1200000.0, ""},
		{field("money"), "2 crore", 2000000000.0, ""},
		{field("money"), "99.999", 10000.0, ""},
		{field("money"), "2000 per month", 200000.0, ""},
		{field("money"), "2k/month", 200000.0, ""},
		{field("money"), "₹24,000 p.a.", 2400000.0, ""},
		{field("money"), "5000 quarterly", 500000.0, ""},
		{field("money"), "1500pm", 150000.0, ""},
		{field("money"), "2 lakh per annum", 20000000.0, ""},
		{field("money"), "lots", nil, "I need a number"},
		{field("money"), "12 bananas", nil, `"bananas"`},
		{field("number"), "7.5", 7.5, ""},

		{field("date"), "2026-12-31", "2026-12-31", ""},
		{field("date"), "15/11/2026", "2026-11-15", ""},
		{field("date"), "15-11-26", "2026-11-15", ""},
		{field("date"), "15/11", "2026-11-15", ""},
		{field("date"), "15/01", "2027-01-15", ""}, // already passed this year → next year
		{field("date"), "15 Nov", "2026-11-15", ""},
		{field("date"), "Nov 15, 2026", "2026-11-15", ""},
		{field("date"), "today", "2026-10-31", ""},
		{field("date"), "tomorrow", "2026-11-01", ""},
		{field("date"), "friday", "2026-11-06", ""},
		{field("date"), "next saturday", "2026-11-07", ""},
		{field("date"), "31/02/2026", nil, "I need a date"},
		{field("date"), "soon", nil, "I need a date"},

		{choice, "Pass", "pass", ""},
		{choice, "passed", "pass", ""},
		{choice, "f", "fail", ""},
		{choice, "maybe", nil, "one of: pass, fail"},
		{field("bool"), "Yes!", true, ""},
		{field("bool"), "n", false, ""},
		{field("bool"), "perhaps", nil, "yes or no"},

		{client, "acme", "acme", ""},         // exact key, even though "Acme Exports" also contains it
		{client, "acme traders", "acme", ""}, // exact name
		{client, "bolt", "bolt", ""},
		{client, "exports", "acme-exports", ""},       // the only name containing it
		{client, "Acm", nil, "which one do you mean"}, // two partial matches
		{client, "zeta", nil, `called "zeta"`},
		{field("person"), "mia", "p-mia", ""},
		{field("person"), "o", nil, `called "o"`}, // too short to guess from

		{people, "Mia, Olga and Omar", []any{"p-mia", "p-olga", "p-omar"}, ""},
		{playbook.Field{Key: "x", Type: "list", Of: "text"}, "Bank statements; invoices", []any{"Bank statements", "invoices"}, ""},
		{field("text"), "  Monthly books  ", "Monthly books", ""},
		{field("text"), "   ", nil, "empty"},
	}
	for _, tt := range tests {
		got, err := Understand(tt.field, tt.answer, today, fakeChoices{})
		switch {
		case tt.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s %q: got %v, %v; want an error mentioning %q", tt.field.Type, tt.answer, got, err, tt.wantErr)
			}
		case err != nil:
			t.Errorf("%s %q: %v", tt.field.Type, tt.answer, err)
		case !reflect.DeepEqual(got, tt.want):
			t.Errorf("%s %q = %#v, want %#v", tt.field.Type, tt.answer, got, tt.want)
		}
	}
}

func TestCommand(t *testing.T) {
	words := []string{"submit", "cancel", "approve", "reject", "send_to_ops"}
	tests := []struct {
		text, word, reason string
		ok                 bool
	}{
		{"submit", "submit", "", true},
		{"Submit", "submit", "", true},
		{"cancel because the client withdrew", "cancel", "the client withdrew", true},
		{"reject: fee too low", "reject", "fee too low", true},
		{"reject as assets are missing", "reject", "assets are missing", true},
		{"submitted the papers", "", "", false}, // not the word "submit"
		{"please submit", "", "", false},
		{"send to ops", "send_to_ops", "", true},
		{"Send_to_ops", "send_to_ops", "", true},
		{"send to ops: fee to be confirmed", "send_to_ops", "fee to be confirmed", true},
	}
	for _, tt := range tests {
		word, reason, ok := Command(tt.text, words)
		if word != tt.word || reason != tt.reason || ok != tt.ok {
			t.Errorf("Command(%q) = %q, %q, %v; want %q, %q, %v", tt.text, word, reason, ok, tt.word, tt.reason, tt.ok)
		}
	}
}

func TestQuestions(t *testing.T) {
	p, err := playbook.LoadFile("../testdata/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := NextField(p, "draft", nil)
	if f == nil || f.Key != "client" || Question(*f) != "Which client is this for?" {
		t.Errorf("first question = %+v", f)
	}
	integrity := NextField(p, "draft", map[string]any{"client": "a", "nature_of_work": "b", "reason": "c", "fee_agreed": 1.0})
	if integrity == nil || !strings.HasSuffix(Question(*integrity), "(pass / fail)") {
		t.Errorf("integrity question = %v", integrity)
	}
	if NextField(p, "draft", map[string]any{"client": "a", "nature_of_work": "b", "reason": "c", "fee_agreed": 1.0,
		"integrity_check": "pass"}) != nil {
		t.Error("asked something although every required field is filled")
	}
}
