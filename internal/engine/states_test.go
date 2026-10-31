// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"errors"
	"slices"
	"testing"
)

// complete returns fields where every required field of the sample playbook is filled in.
func complete() Fields {
	return Fields{
		"client":          "acme",
		"nature_of_work":  "bookkeeping",
		"reason":          "Monthly books",
		"fee_agreed":      1200000.0,
		"integrity_check": "pass",
	}
}

func TestMissing(t *testing.T) {
	p := loadExample(t)
	want := []string{"client", "nature_of_work", "reason", "fee_agreed", "integrity_check"}
	if got := Missing(p, Fields{}); !slices.Equal(got, want) {
		t.Errorf("Missing(empty) = %v, want %v", got, want)
	}
	if got := Missing(p, complete()); len(got) != 0 {
		t.Errorf("Missing(complete) = %v, want none", got)
	}
}

func TestTransition(t *testing.T) {
	p := loadExample(t)
	partial := complete()
	delete(partial, "fee_agreed")

	tests := []struct {
		name, state, action, reason string
		fields                      Fields
		wantTo                      string
		wantErr                     error
	}{
		{"submit a complete draft", "draft", "submit", "", complete(), "pending_approval", nil},
		{"submit with a field missing", "draft", "submit", "", partial, "", &MissingError{}},
		{"submit when already approved", "approved", "submit", "", complete(), "", ErrNotNow},
		{"cancel without a reason", "draft", "cancel", " ", complete(), "", ErrReasonNeeded},
		{"cancel an approved item", "approved", "cancel", "Client withdrew", complete(), "cancelled", nil},
		{"cancel twice", "cancelled", "cancel", "again", complete(), "", ErrNotNow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			to, err := Transition(p, Item{State: tt.state, Fields: tt.fields}, tt.action, tt.reason)
			switch want := tt.wantErr.(type) {
			case nil:
				if err != nil || to != tt.wantTo {
					t.Errorf("Transition = %q, %v; want %q", to, err, tt.wantTo)
				}
			case *MissingError:
				var missing *MissingError
				if !errors.As(err, &missing) || !slices.Equal(missing.Keys, []string{"fee_agreed"}) {
					t.Errorf("Transition error = %v, want still needed: fee_agreed", err)
				}
			default:
				if !errors.Is(err, want) {
					t.Errorf("Transition error = %v, want %v", err, want)
				}
			}
		})
	}

	if _, err := Transition(p, Item{State: "draft"}, "approve-everything", ""); err == nil {
		t.Error("Transition accepted an unknown action")
	}
}

func TestActionsAndClosed(t *testing.T) {
	p := loadExample(t)
	tests := []struct {
		state      string
		wantAction []string
		wantClosed bool
	}{
		{"draft", []string{"submit", "cancel"}, false},
		{"pending_approval", []string{"cancel"}, false},
		{"approved", []string{"cancel"}, false},
		{"cancelled", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := Actions(p, Item{State: tt.state}); !slices.Equal(got, tt.wantAction) {
				t.Errorf("Actions = %v, want %v", got, tt.wantAction)
			}
			if got := IsClosed(p, tt.state); got != tt.wantClosed {
				t.Errorf("IsClosed = %v, want %v", got, tt.wantClosed)
			}
		})
	}
}

func TestCheckSet(t *testing.T) {
	p := loadExample(t)
	draft := Item{State: "draft"}

	tests := []struct {
		name    string
		item    Item
		key     string
		value   any
		wantErr bool
	}{
		{"fee in minor units", draft, "fee_agreed", 1200000.0, false},
		{"fee with a fraction", draft, "fee_agreed", 12.5, true},
		{"fee as text", draft, "fee_agreed", "twelve", true},
		{"date in ISO form", draft, "client_data_due", "2026-11-15", false},
		{"date in another form", draft, "client_data_due", "15/11/2026", true},
		{"allowed choice", draft, "integrity_check", "fail", false},
		{"choice not in options", draft, "integrity_check", "maybe", true},
		{"list of text", draft, "required_documents", []any{"Bank statements", "Invoices"}, false},
		{"list with a number in it", draft, "required_documents", []any{"Bank statements", 3.0}, true},
		{"empty person", draft, "maker", "", true},
		{"clearing a field", draft, "advance", nil, false},
		{"readonly field", draft, "standard_fee", 100.0, true},
		{"unknown field", draft, "discount", 10.0, true},
		{"cancelled item", Item{State: "cancelled"}, "reason", "changed my mind", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSet(p, tt.item, tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckSet(%s = %v) error = %v, wantErr %v", tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}
