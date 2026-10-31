// SPDX-License-Identifier: AGPL-3.0-or-later

package master

import (
	"strings"
	"testing"

	"seyal/internal/engine"
	"seyal/internal/playbook"
)

func workTypeKind(t *testing.T) *playbook.MasterKind {
	t.Helper()
	k, err := playbook.LoadMasterKindFile("../../examples/masters/work_type.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCheck(t *testing.T) {
	kind := workTypeKind(t)
	tests := []struct {
		name, key, itemName string
		fields              engine.Fields
		wantErr             string // "" means valid
	}{
		{"valid", "monthly-books", "Monthly bookkeeping",
			engine.Fields{"standard_fee": 1200000.0, "documents": []any{"Bank statements"}, "is_new": false}, ""},
		{"empty value is dropped", "monthly-books", "Monthly bookkeeping", engine.Fields{"est_hours": nil}, ""},
		{"bad key", "Monthly Books", "Monthly bookkeeping", nil, "key"},
		{"no name", "monthly-books", "  ", nil, "name is required"},
		{"unknown field", "monthly-books", "Monthly bookkeeping", engine.Fields{"colour": "red"}, `no field "colour"`},
		{"wrong type", "monthly-books", "Monthly bookkeeping", engine.Fields{"standard_fee": "lots"}, "standard_fee must be"},
		{"bool as text", "monthly-books", "Monthly bookkeeping", engine.Fields{"is_new": "yes"}, "is_new must be yes or no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, err := check(kind, tt.key, tt.itemName, tt.fields)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("check = %v, want valid", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("check = %v, want an error mentioning %q", err, tt.wantErr)
			case tt.wantErr == "":
				for k, v := range clean {
					if v == nil {
						t.Errorf("empty value %s was kept", k)
					}
				}
			}
		})
	}
}
