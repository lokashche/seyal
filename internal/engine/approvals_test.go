// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

var (
	sales = Person{ID: "p-sales", Roles: []string{"sales"}}
	ops   = Person{ID: "p-ops", Roles: []string{"ops"}}
	boss  = Person{ID: "p-owner", Roles: []string{"owner", "sales"}}
)

// waiting returns a job at version 3 that is waiting for approvals. Its fee equals the standard
// fee, so only Sales and Ops must approve. change, if given, edits the fields first.
func waiting(t *testing.T, change func(Fields)) Item {
	t.Helper()
	f := complete()
	f["standard_fee"] = f["fee_agreed"]
	if change != nil {
		change(f)
	}
	return Item{State: "pending_approval", Version: 3, Fields: f}
}

// decided returns person's decision on slot for the item's current version.
func decided(t *testing.T, it Item, slot string, by Person, decision string) Approval {
	t.Helper()
	hash, err := HashFields(it.Fields)
	if err != nil {
		t.Fatal(err)
	}
	return Approval{Slot: slot, PersonID: by.ID, Decision: decision, ItemVersion: it.Version, FieldsHash: hash}
}

func TestRequiredApprovals(t *testing.T) {
	p := loadExample(t)
	tests := []struct {
		name   string
		change func(Fields)
		want   []string
	}{
		{"normal job", nil, []string{"sales", "ops"}},
		{"fee below standard", func(f Fields) { f["fee_agreed"] = 900000.0 }, []string{"sales", "ops", "owner"}},
		{"integrity failed", func(f Fields) { f["integrity_check"] = "fail" }, []string{"sales", "ops", "owner"}},
		{"new work type", func(f Fields) { f["nature_of_work"] = "new-service" }, []string{"sales", "ops", "owner"}},
		{"unclear: work type has no is_new", func(f Fields) { f["nature_of_work"] = "unflagged" }, []string{"sales", "ops", "owner"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := Status(p, waiting(t, tt.change), nil, lookup)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(st.Waiting, tt.want) {
				t.Errorf("waiting for %v, want %v", st.Waiting, tt.want)
			}
			if st.Next != "" {
				t.Errorf("Next = %q with nobody approved, want to stay", st.Next)
			}
		})
	}

	st, _ := Status(p, waiting(t, func(f Fields) { f["nature_of_work"] = "unflagged" }), nil, lookup)
	if why := st.Slots[2].Why; !strings.Contains(why, "missing") {
		t.Errorf("owner slot Why = %q, want it to say information is missing", why)
	}
}

func TestApprovalOutcome(t *testing.T) {
	p := loadExample(t)
	job := waiting(t, nil)

	t.Run("sales and ops approve", func(t *testing.T) {
		given := []Approval{decided(t, job, "sales", sales, Approve), decided(t, job, "ops", ops, Approve)}
		st, err := Status(p, job, given, lookup)
		if err != nil || len(st.Waiting) != 0 || st.Next != "approved" {
			t.Errorf("Status = %+v, %v; want approved", st, err)
		}
	})

	t.Run("approvals of an older version don't count", func(t *testing.T) {
		old := job
		old.Version = 2
		given := []Approval{decided(t, old, "sales", sales, Approve), decided(t, old, "ops", ops, Approve)}
		st, _ := Status(p, job, given, lookup)
		if len(st.Waiting) != 2 || st.Next != "" {
			t.Errorf("Status = %+v, want still waiting for sales and ops", st)
		}
	})

	t.Run("approvals of different fields don't count", func(t *testing.T) {
		before := waiting(t, func(f Fields) { f["advance"] = 100000.0 })
		given := []Approval{decided(t, before, "sales", sales, Approve)}
		st, _ := Status(p, job, given, lookup)
		if !slices.Contains(st.Waiting, "sales") {
			t.Errorf("Status = %+v, want sales still waiting", st)
		}
	})

	t.Run("a rejection sends it back", func(t *testing.T) {
		given := []Approval{decided(t, job, "sales", sales, Approve), decided(t, job, "ops", ops, Reject)}
		st, _ := Status(p, job, given, lookup)
		if st.Rejected == nil || st.Next != "draft" {
			t.Errorf("Status = %+v, want rejected and back to draft", st)
		}
	})
}

func TestCheckDecision(t *testing.T) {
	p := loadExample(t)
	job := waiting(t, nil)
	salesDone := []Approval{decided(t, job, "sales", sales, Approve)}

	tests := []struct {
		name     string
		item     Item
		by       Person
		slot     string
		decision string
		reason   string
		given    []Approval
		wantErr  error // nil means allowed; errAny means any error
	}{
		{"sales approves", job, sales, "sales", Approve, "", nil, nil},
		{"ops rejects with a reason", job, ops, "ops", Reject, "Fee too low for the scope", nil, nil},
		{"ops rejects without a reason", job, ops, "ops", Reject, "", nil, ErrReasonNeeded},
		{"wrong role", job, ops, "sales", Approve, "", nil, ErrNotAllowed},
		{"slot not required for this job", job, boss, "owner", Approve, "", nil, errAny},
		{"slot decided already", job, boss, "sales", Approve, "", salesDone, errAny},
		{"not waiting for approvals", Item{State: "draft", Version: 3, Fields: job.Fields}, sales, "sales", Approve, "", nil, ErrNotNow},
		{"unknown slot", job, sales, "finance", Approve, "", nil, errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckDecision(p, tt.item, tt.by, tt.slot, tt.decision, tt.reason, tt.given, lookup)
			switch tt.wantErr {
			case nil:
				if err != nil {
					t.Errorf("CheckDecision = %v, want allowed", err)
				}
			case errAny:
				if err == nil {
					t.Error("CheckDecision allowed it, want an error")
				}
			default:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("CheckDecision = %v, want %v", err, tt.wantErr)
				}
			}
		})
	}
}

func TestOnePersonOneApproval(t *testing.T) {
	p := loadExample(t)
	// The fee is below standard, so the owner must approve too. The boss holds both the owner and
	// the sales role, but may fill only one of the two slots.
	job := waiting(t, func(f Fields) { f["fee_agreed"] = 900000.0 })
	given := []Approval{decided(t, job, "sales", boss, Approve)}

	err := CheckDecision(p, job, boss, "owner", Approve, "", given, lookup)
	if !errors.Is(err, ErrNotAllowed) {
		t.Errorf("same person approving twice = %v, want ErrNotAllowed", err)
	}
	if err := CheckDecision(p, job, Person{ID: "p-owner-2", Roles: []string{"owner"}}, "owner", Approve, "", given, lookup); err != nil {
		t.Errorf("a second owner approving = %v, want allowed", err)
	}
}

func TestStateAfterEdit(t *testing.T) {
	p := loadExample(t)
	for state, want := range map[string]string{
		"approved":         "pending_approval",
		"pending_approval": "pending_approval",
		"draft":            "draft",
	} {
		if got := StateAfterEdit(p, state); got != want {
			t.Errorf("StateAfterEdit(%s) = %s, want %s", state, got, want)
		}
	}
}

func TestHashFields(t *testing.T) {
	a, _ := HashFields(Fields{"x": 1.0, "y": "b"})
	b, _ := HashFields(Fields{"y": "b", "x": 1.0})
	c, _ := HashFields(Fields{"x": 2.0, "y": "b"})
	if a != b {
		t.Error("the same fields in a different order gave a different fingerprint")
	}
	if a == c {
		t.Error("changing a field did not change the fingerprint")
	}
}

// errAny is a marker for "any error is fine" in test tables.
var errAny = errors.New("any error")
