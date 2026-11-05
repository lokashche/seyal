// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engine applies a playbook's rules to a work item. It is pure Go, with no database
// and no network, so every rule can be tested quickly.
package engine

import (
	"fmt"
	"strings"

	"seyal/internal/playbook"
)

// Fields holds a work item's field values as decoded from JSON: string, float64, bool,
// []any or nil. Master fields hold the key of the chosen master item.
type Fields map[string]any

// MasterLookup returns the fields of one master-data item, or false if there is none.
type MasterLookup func(kind, key string) (Fields, bool)

// Result is the answer to a condition. Unknown means a value it needs is missing, so callers
// can choose the safe option (for approvals: ask for the approval).
type Result int

const (
	False Result = iota
	True
	Unknown
)

func (r Result) String() string {
	return [...]string{"false", "true", "unknown"}[r]
}

// Eval answers condition c for the given fields. It returns an error only for a condition that
// can never be answered, such as "less than" between text and a number.
func Eval(p *playbook.Playbook, c playbook.Condition, fields Fields, lookup MasterLookup) (Result, error) {
	e := evaluator{p: p, fields: fields, lookup: lookup}
	return e.eval(c)
}

type evaluator struct {
	p      *playbook.Playbook
	fields Fields
	lookup MasterLookup
}

func (e evaluator) eval(c playbook.Condition) (Result, error) {
	switch {
	case len(c.Any) > 0:
		answer := False
		for _, sub := range c.Any {
			r, err := e.eval(sub)
			if err != nil || r == True {
				return r, err
			}
			if r == Unknown {
				answer = Unknown
			}
		}
		return answer, nil
	case len(c.All) > 0:
		answer := True
		for _, sub := range c.All {
			r, err := e.eval(sub)
			if err != nil || r == False {
				return r, err
			}
			if r == Unknown {
				answer = Unknown
			}
		}
		return answer, nil
	}

	if c.Filled != nil { // "is it filled in?" always has an answer
		return toResult(!isEmpty(e.value(c.Field)) == *c.Filled), nil
	}
	op, want := operator(c)
	if want == nil {
		return False, fmt.Errorf("engine: condition on %q has no comparison", c.Field)
	}
	left, right := e.value(c.Field), want.Literal
	if want.Field != "" {
		right = e.value(want.Field)
	}
	if isEmpty(left) || isEmpty(right) {
		if c.IfMissing != nil { // the process says what a missing value means
			return toResult(*c.IfMissing), nil
		}
		return Unknown, nil
	}

	switch op {
	case "eq", "ne":
		same := equal(left, right)
		return toResult(same == (op == "eq")), nil
	default: // lt, gt
		cmp, ok := compare(left, right)
		if !ok {
			return False, fmt.Errorf("engine: cannot compare %v with %v in condition on %q", left, right, c.Field)
		}
		return toResult((op == "lt" && cmp < 0) || (op == "gt" && cmp > 0)), nil
	}
}

// value returns the value a reference points at: a field ("fee_agreed") or a field of the
// chosen master item ("nature_of_work.is_new"). It returns nil when there is none.
func (e evaluator) value(ref string) any {
	key, sub, dotted := strings.Cut(ref, ".")
	v := e.fields[key]
	if !dotted {
		return v
	}
	itemKey, ok := v.(string)
	if !ok || itemKey == "" || e.lookup == nil {
		return nil
	}
	for _, f := range e.p.Fields {
		if f.Key == key {
			if item, found := e.lookup(f.Kind, itemKey); found {
				return item[sub]
			}
		}
	}
	return nil
}

func operator(c playbook.Condition) (string, *playbook.Value) {
	switch {
	case c.Eq != nil:
		return "eq", c.Eq
	case c.Ne != nil:
		return "ne", c.Ne
	case c.Lt != nil:
		return "lt", c.Lt
	case c.Gt != nil:
		return "gt", c.Gt
	}
	return "", nil
}

// IsEmpty reports whether a value counts as "not filled in": nothing, "" or an empty list.
func IsEmpty(v any) bool { return isEmpty(v) }

// isEmpty reports whether a value counts as "not filled in".
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}
	return false
}

// number converts the number types that JSON and YAML produce into a float64.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// equal compares two values; 100 (from YAML) equals 100.0 (from JSON).
func equal(a, b any) bool {
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

// SameValue reports whether two stored field values are the same, lists included; two empty values
// are the same.
func SameValue(a, b any) bool {
	if isEmpty(a) || isEmpty(b) {
		return isEmpty(a) && isEmpty(b)
	}
	ha, errA := HashFields(Fields{"v": a})
	hb, errB := HashFields(Fields{"v": b})
	return errA == nil && errB == nil && ha == hb
}

// compare orders two numbers, or two texts such as ISO dates ("2026-10-31").
func compare(a, b any) (int, bool) {
	if x, ok := number(a); ok {
		y, ok := number(b)
		switch {
		case !ok:
			return 0, false
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
		return 0, true
	}
	x, ok1 := a.(string)
	y, ok2 := b.(string)
	if !ok1 || !ok2 {
		return 0, false
	}
	return strings.Compare(x, y), true
}

func toResult(b bool) Result {
	if b {
		return True
	}
	return False
}
