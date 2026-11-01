// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

const examplePath = "../testdata/playbooks/job-evaluation.yaml"

func TestLoadExample(t *testing.T) {
	p, err := LoadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "job-evaluation" || p.Version != "0.1" {
		t.Errorf("id/version = %q/%q", p.ID, p.Version)
	}
	if len(p.Fields) != 17 {
		t.Errorf("got %d fields, want 17", len(p.Fields))
	}
	if got := p.Fields[1].Prefill["standard_fee"]; got != "standard_fee" {
		t.Errorf("nature_of_work prefills standard_fee from %q", got)
	}

	owner := p.Approvals.Slots[2]
	if owner.Slot != "owner" || owner.RequiredIf == nil || len(owner.RequiredIf.Any) != 3 {
		t.Fatalf("owner slot not parsed: %+v", owner)
	}
	feeRule := owner.RequiredIf.Any[0]
	if feeRule.Field != "fee_agreed" || feeRule.Lt == nil || feeRule.Lt.Field != "standard_fee" {
		t.Errorf("fee rule = %+v, want fee_agreed < field standard_fee", feeRule)
	}
	integrity := owner.RequiredIf.Any[2]
	if integrity.Eq == nil || integrity.Eq.Literal != "fail" {
		t.Errorf("integrity rule = %+v, want eq fail", integrity)
	}

	if acts := p.On["approved"]; len(acts) != 2 || acts[1].Do != "assign" || acts[1].To != "maker" {
		t.Errorf("on approved = %+v", acts)
	}
	if p.Hash == [32]byte{} {
		t.Error("hash not set")
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"empty file", "", "empty"},
		{"typo in a key", "id: x\ntitel: oops\n", "titel"},
		{"typo inside a field", "fields:\n  - { key: a, tpye: text }\n", "tpye"},
		{"bad value form", "approvals:\n  slots:\n    - required_if: { field: a, eq: { name: b } }\n", "plain value or { field: name }"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
