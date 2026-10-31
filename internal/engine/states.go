// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"seyal/internal/playbook"
)

// Item is the part of a work item the engine needs to decide things.
type Item struct {
	State   string
	Version int
	Fields  Fields
}

var (
	// ErrNotNow means the action exists but isn't possible in the item's current state.
	ErrNotNow = errors.New("not possible in the current state")
	// ErrReasonNeeded means the action needs a reason, e.g. cancelling.
	ErrReasonNeeded = errors.New("a reason is required")
	// ErrClosed means the item can no longer be changed.
	ErrClosed = errors.New("the item is closed")
)

// MissingError lists the required fields that still have no value.
type MissingError struct {
	Keys []string
}

func (e *MissingError) Error() string {
	return "still needed: " + strings.Join(e.Keys, ", ")
}

// Missing returns the keys of required fields that have no value yet, in playbook order.
func Missing(p *playbook.Playbook, fields Fields) []string {
	var keys []string
	for _, f := range p.Fields {
		if f.Required && isEmpty(fields[f.Key]) {
			keys = append(keys, f.Key)
		}
	}
	return keys
}

// Actions returns the actions that can start from the item's current state, in playbook order.
// Transition still checks their other conditions (required fields, reason).
func Actions(p *playbook.Playbook, it Item) []string {
	var actions []string
	for _, t := range p.Transitions {
		if slices.Contains(t.From, it.State) {
			actions = append(actions, t.Action)
		}
	}
	return actions
}

// Transition checks whether action may happen to the item now, and returns the state it moves
// to. reason is the reason the person gave, if any.
func Transition(p *playbook.Playbook, it Item, action, reason string) (string, error) {
	i := slices.IndexFunc(p.Transitions, func(t playbook.Transition) bool { return t.Action == action })
	if i < 0 {
		return "", fmt.Errorf("engine: unknown action %q", action)
	}
	t := p.Transitions[i]
	if !slices.Contains(t.From, it.State) {
		return "", fmt.Errorf("%s while %s: %w", action, it.State, ErrNotNow)
	}
	if t.When == "required_filled" {
		if keys := Missing(p, it.Fields); len(keys) > 0 {
			return "", &MissingError{Keys: keys}
		}
	}
	if t.Needs == "reason" && strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("%s: %w", action, ErrReasonNeeded)
	}
	return t.To, nil
}

// IsClosed reports whether nothing more can happen in this state, e.g. cancelled.
func IsClosed(p *playbook.Playbook, state string) bool {
	if p.Approvals != nil && p.Approvals.In == state {
		return false
	}
	for _, t := range p.Transitions {
		if slices.Contains(t.From, state) {
			return false
		}
	}
	return true
}

// CheckSet checks that a person may set field key to v on the item now. A nil v clears the field.
func CheckSet(p *playbook.Playbook, it Item, key string, v any) error {
	i := slices.IndexFunc(p.Fields, func(f playbook.Field) bool { return f.Key == key })
	switch {
	case i < 0:
		return fmt.Errorf("engine: unknown field %q", key)
	case p.Fields[i].Readonly:
		return fmt.Errorf("%s is filled in automatically and can't be changed", key)
	case IsClosed(p, it.State):
		return fmt.Errorf("set %s: %w", key, ErrClosed)
	case v == nil:
		return nil
	}
	return CheckValue(p.Fields[i], v)
}

// CheckValue checks that v fits field f's type. Values are as decoded from JSON.
func CheckValue(f playbook.Field, v any) error {
	bad := func(want string) error { return fmt.Errorf("%s must be %s, got %v", f.Key, want, v) }
	switch f.Type {
	case "text":
		if _, ok := v.(string); !ok {
			return bad("text")
		}
	case "person", "master":
		if s, ok := v.(string); !ok || s == "" {
			return bad("a chosen " + f.Type)
		}
	case "number":
		if _, ok := number(v); !ok {
			return bad("a number")
		}
	case "money":
		// Money is stored in minor units (e.g. cents), so it is always a whole number.
		if x, ok := number(v); !ok || x != math.Trunc(x) {
			return bad("a whole number of minor units")
		}
	case "date":
		s, ok := v.(string)
		if !ok {
			return bad("a date like 2026-12-31")
		}
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			return bad("a date like 2026-12-31")
		}
	case "choice":
		if s, ok := v.(string); !ok || !slices.Contains(f.Options, s) {
			return bad("one of " + strings.Join(f.Options, ", "))
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return bad("yes or no")
		}
	case "list":
		items, ok := v.([]any)
		if !ok {
			return bad("a list")
		}
		for i, item := range items {
			element := playbook.Field{Key: fmt.Sprintf("%s[%d]", f.Key, i), Type: f.Of}
			if err := CheckValue(element, item); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("engine: field %s has unknown type %q", f.Key, f.Type)
	}
	return nil
}
