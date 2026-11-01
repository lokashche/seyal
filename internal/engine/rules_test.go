// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"testing"

	"seyal/internal/playbook"
)

const examplePath = "../testdata/playbooks/job-evaluation.yaml"

// workTypes is fake master data for the tests.
var workTypes = map[string]Fields{
	"bookkeeping": {"is_new": false},
	"new-service": {"is_new": true},
	"unflagged":   {},
}

func lookup(kind, key string) (Fields, bool) {
	if kind != "work_type" {
		return nil, false
	}
	f, ok := workTypes[key]
	return f, ok
}

func loadExample(t *testing.T) *playbook.Playbook {
	t.Helper()
	p, err := playbook.LoadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOwnerRule(t *testing.T) {
	p := loadExample(t)
	ownerRule := *p.Approvals.Slots[2].RequiredIf

	// A normal job: fee at standard, known work type, integrity passed.
	normal := Fields{"fee_agreed": 12000.0, "standard_fee": 12000.0, "nature_of_work": "bookkeeping", "integrity_check": "pass"}
	with := func(key string, v any) Fields {
		f := Fields{}
		for k, x := range normal {
			f[k] = x
		}
		f[key] = v
		return f
	}

	tests := []struct {
		name   string
		fields Fields
		want   Result
	}{
		{"normal job needs no owner", normal, False},
		{"fee below standard", with("fee_agreed", 9000.0), True},
		{"fee above standard", with("fee_agreed", 15000.0), False},
		{"new work type", with("nature_of_work", "new-service"), True},
		{"integrity failed", with("integrity_check", "fail"), True},
		{"fee not entered yet", with("fee_agreed", nil), Unknown},
		{"work type not found", with("nature_of_work", "no-such-type"), Unknown},
		{"work type without is_new", with("nature_of_work", "unflagged"), Unknown},
		{"one rule true beats unknown", Fields{"fee_agreed": 9000.0, "standard_fee": 12000.0}, True},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(p, ownerRule, tt.fields, lookup)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("owner rule = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComparisons(t *testing.T) {
	p := loadExample(t)
	leaf := func(field, op string, v any) playbook.Condition {
		val := &playbook.Value{Literal: v}
		c := playbook.Condition{Field: field}
		switch op {
		case "eq":
			c.Eq = val
		case "ne":
			c.Ne = val
		case "lt":
			c.Lt = val
		case "gt":
			c.Gt = val
		}
		return c
	}

	tests := []struct {
		name   string
		cond   playbook.Condition
		fields Fields
		want   Result
	}{
		{"YAML 100 equals JSON 100.0", leaf("fee_agreed", "eq", 100), Fields{"fee_agreed": 100.0}, True},
		{"not equal", leaf("integrity_check", "ne", "fail"), Fields{"integrity_check": "pass"}, True},
		{"earlier date", leaf("client_data_due", "lt", "2026-12-01"), Fields{"client_data_due": "2026-11-15"}, True},
		{"later date", leaf("client_data_due", "gt", "2026-12-01"), Fields{"client_data_due": "2026-11-15"}, False},
		{"empty text is unknown", leaf("reason", "eq", "x"), Fields{"reason": ""}, Unknown},
		{"text never equals a number", leaf("reason", "eq", 5), Fields{"reason": "5"}, False},
		{"all with one false", playbook.Condition{All: []playbook.Condition{
			leaf("fee_agreed", "gt", 0), leaf("integrity_check", "eq", "pass")}},
			Fields{"fee_agreed": 10.0, "integrity_check": "fail"}, False},
		{"all with one unknown", playbook.Condition{All: []playbook.Condition{
			leaf("fee_agreed", "gt", 0), leaf("integrity_check", "eq", "pass")}},
			Fields{"fee_agreed": 10.0}, Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(p, tt.cond, tt.fields, lookup)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Eval = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCannotCompareTextWithNumber(t *testing.T) {
	p := loadExample(t)
	c := playbook.Condition{Field: "reason", Lt: &playbook.Value{Literal: 5}}
	if _, err := Eval(p, c, Fields{"reason": "abc"}, lookup); err == nil {
		t.Error("Eval compared text with a number without an error")
	}
}
