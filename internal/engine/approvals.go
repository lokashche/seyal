// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"seyal/internal/playbook"
)

// Decisions a person can make on an approval slot.
const (
	Approve = "approve"
	Reject  = "reject"
)

// ErrNotAllowed means the person may not do this, e.g. they lack the role.
var ErrNotAllowed = errors.New("not allowed for this person")

// Approval is one decision a person made on one approval slot of a work item.
type Approval struct {
	Slot        string
	PersonID    string
	Decision    string // Approve or Reject
	ItemVersion int
	FieldsHash  string // HashFields of the item when the decision was made
}

// Person is someone acting on a work item, with the roles they hold.
type Person struct {
	ID    string
	Roles []string
}

// SlotStatus describes one approval slot for the item's current version.
type SlotStatus struct {
	Slot, Role string
	Required   bool
	Why        string // why it is or isn't required, for people to read
	ApprovedBy string // person id, if approved for the current version
}

// ApprovalStatus summarises the approvals of the item's current version.
type ApprovalStatus struct {
	Slots    []SlotStatus
	Waiting  []string  // required slots that have no approval yet
	Rejected *Approval // a rejection of the current version, if any
	Next     string    // the state the item should move to now, or "" to stay
}

// HashFields fingerprints an item's fields. Approvals are bound to this fingerprint, so any change
// to any field makes earlier approvals stop counting. (json.Marshal sorts map keys, so the same
// fields always give the same fingerprint.)
func HashFields(f Fields) (string, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Status works out which approvals the item needs now and which of the given decisions count.
// Only decisions made on the item's current version and fields count.
func Status(p *playbook.Playbook, it Item, given []Approval, lookup MasterLookup) (ApprovalStatus, error) {
	var st ApprovalStatus
	a := p.Approvals
	if a == nil {
		return st, nil
	}
	current, err := currentDecisions(it, given)
	if err != nil {
		return st, err
	}

	for _, s := range a.Slots {
		ss := SlotStatus{Slot: s.Slot, Role: s.Role, Required: true, Why: "always required"}
		if s.RequiredIf != nil {
			r, err := Eval(p, *s.RequiredIf, it.Fields, lookup)
			if err != nil {
				return st, fmt.Errorf("approval slot %s: %w", s.Slot, err)
			}
			switch r {
			case True:
				ss.Why = "required by the playbook's rule"
			case Unknown:
				ss.Why = "required because some information is missing"
			case False:
				ss.Required, ss.Why = false, "not required for this item"
			}
		}
		for _, d := range current {
			if d.Slot == s.Slot && d.Decision == Approve {
				ss.ApprovedBy = d.PersonID
			}
		}
		if ss.Required && ss.ApprovedBy == "" {
			st.Waiting = append(st.Waiting, s.Slot)
		}
		st.Slots = append(st.Slots, ss)
	}

	for i := range current {
		if current[i].Decision == Reject {
			st.Rejected = &current[i]
			break
		}
	}
	if it.State == a.In {
		switch {
		case st.Rejected != nil:
			st.Next = a.OnReject
		case len(st.Waiting) == 0:
			st.Next = a.OnComplete
		}
	}
	return st, nil
}

// CheckDecision checks that a person may approve or reject a slot on the item now:
//   - the item must be waiting for approvals, and the slot must be required;
//   - the person must hold the slot's role;
//   - each slot is decided once per version;
//   - one person makes at most one decision per version, so different people always approve;
//   - a rejection needs a reason.
func CheckDecision(p *playbook.Playbook, it Item, by Person, slot, decision, reason string,
	given []Approval, lookup MasterLookup) error {
	a := p.Approvals
	if a == nil || it.State != a.In {
		return fmt.Errorf("%s: %w", decision, ErrNotNow)
	}
	if decision != Approve && decision != Reject {
		return fmt.Errorf("engine: unknown decision %q", decision)
	}
	st, err := Status(p, it, given, lookup)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(st.Slots, func(s SlotStatus) bool { return s.Slot == slot })
	if i < 0 {
		return fmt.Errorf("engine: unknown approval slot %q", slot)
	}
	s := st.Slots[i]
	if !s.Required {
		return fmt.Errorf("the %s approval is not required for this item", slot)
	}
	if !slices.Contains(by.Roles, s.Role) {
		return fmt.Errorf("the %s approval needs the %s role: %w", slot, s.Role, ErrNotAllowed)
	}

	current, err := currentDecisions(it, given)
	if err != nil {
		return err
	}
	for _, d := range current {
		if d.Slot == slot {
			return fmt.Errorf("the %s approval has already been decided for this version", slot)
		}
		if d.PersonID == by.ID {
			return fmt.Errorf("you already decided the %s approval of this version, so someone else must decide %s: %w",
				d.Slot, slot, ErrNotAllowed)
		}
	}
	if decision == Reject && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("reject: %w", ErrReasonNeeded)
	}
	return nil
}

// StateAfterEdit returns the state an item moves to when its fields change. An approved item goes
// back to waiting for approvals, because its approvals no longer count.
func StateAfterEdit(p *playbook.Playbook, state string) string {
	if a := p.Approvals; a != nil && state == a.OnComplete {
		return a.In
	}
	return state
}

// currentDecisions keeps only the decisions made on the item's current version and fields.
func currentDecisions(it Item, given []Approval) ([]Approval, error) {
	hash, err := HashFields(it.Fields)
	if err != nil {
		return nil, err
	}
	var current []Approval
	for _, d := range given {
		if d.ItemVersion == it.Version && d.FieldsHash == hash {
			current = append(current, d)
		}
	}
	return current, nil
}
