// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ask lets Seyal fill a work item by asking in chat. It picks the next question from the
// playbook, understands the answer by the field's type, and turns short commands such as "submit"
// into actions.
package ask

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"seyal/internal/engine"
	"seyal/internal/playbook"
)

// Choice is something a person can pick: a person or a master-data item.
type Choice struct {
	ID   string // person id or master item key
	Name string
}

// Choices looks up what can be picked for person and master fields.
type Choices interface {
	People() ([]Choice, error)
	Items(kind string) ([]Choice, error) // active items only
}

// NextField returns the next field Seyal should ask for in state (the step's asks, or the required fields),
// or nil when there is nothing to ask.
func NextField(p *playbook.Playbook, state string, fields engine.Fields) *playbook.Field {
	missing := engine.ToAsk(p, state, fields)
	if len(missing) == 0 {
		return nil
	}
	for i := range p.Fields {
		if p.Fields[i].Key == missing[0] {
			return &p.Fields[i]
		}
	}
	return nil
}

// Question is what Seyal asks for a field: the playbook's `ask` text, with a hint about the form
// of answer it understands.
func Question(f playbook.Field) string {
	q := f.Ask
	if q == "" {
		q = fmt.Sprintf("What is the %s?", strings.ToLower(Label(f)))
	}
	switch f.Type {
	case "choice":
		q += " (" + strings.Join(f.Options, " / ") + ")"
	case "bool":
		q += " (yes / no)"
	case "date":
		q += " (for example 15/11 or tomorrow)"
	case "list":
		q += " (separate them with commas)"
	}
	return q
}

// Label is the field's label, or a readable form of its key.
func Label(f playbook.Field) string {
	if f.Label != "" {
		return f.Label
	}
	return strings.ReplaceAll(f.Key, "_", " ")
}

// Understand turns a person's answer into a value for field f, in the form the engine stores.
// today is the date that answers like "tomorrow" count from. Errors say, in plain words, what kind
// of answer was expected.
func Understand(f playbook.Field, answer string, today time.Time, choices Choices) (any, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, errors.New("the answer is empty")
	}
	switch f.Type {
	case "text":
		return answer, nil
	case "number":
		return parseNumber(answer)
	case "money":
		x, err := parseNumber(answer)
		if err != nil {
			return nil, err
		}
		return math.Round(x * 100), nil // stored in minor units (cents, paise)
	case "date":
		return parseDate(answer, today)
	case "choice":
		return pickOption(answer, f.Options)
	case "bool":
		return parseYesNo(answer)
	case "person":
		people, err := choices.People()
		if err != nil {
			return nil, err
		}
		return match(answer, people, "person")
	case "master":
		items, err := choices.Items(f.Kind)
		if err != nil {
			return nil, err
		}
		return match(answer, items, strings.ReplaceAll(f.Kind, "_", " "))
	case "list":
		var list []any
		for _, part := range splitList(answer) {
			v, err := Understand(playbook.Field{Key: f.Key, Type: f.Of}, part, today, choices)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", part, err)
			}
			list = append(list, v)
		}
		if len(list) == 0 {
			return nil, errors.New("the list is empty")
		}
		return list, nil
	}
	return nil, fmt.Errorf("ask: field %s has unknown type %q", f.Key, f.Type)
}

// multipliers are number words people use for amounts, including the Indian lakh and crore.
var multipliers = map[string]float64{
	"k": 1e3, "thousand": 1e3,
	"lakh": 1e5, "lakhs": 1e5, "lac": 1e5, "lacs": 1e5,
	"m": 1e6, "mn": 1e6, "million": 1e6,
	"cr": 1e7, "crore": 1e7, "crores": 1e7,
}

var numberPart = regexp.MustCompile(`^(-?[0-9]*\.?[0-9]+)\s*([a-z]*)$`)

// parseNumber understands "12000", "12,000", "₹12k", "Rs. 1.5 lakh" and "12000/-".
func parseNumber(s string) (float64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "/-")
	for _, symbol := range []string{"₹", "rs.", "rs", "inr", "$", "€", "£"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, symbol))
	}
	s = strings.ReplaceAll(s, ",", "")
	m := numberPart.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("I need a number, like 12000 or 12k")
	}
	x, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, errors.New("I need a number, like 12000 or 12k")
	}
	if unit := m[2]; unit != "" {
		times, ok := multipliers[unit]
		if !ok {
			return 0, fmt.Errorf("I don't know the word %q in a number", unit)
		}
		x *= times
	}
	return x, nil
}

var (
	fullDates = []string{"2006-01-02", "2/1/2006", "2-1-2006", "2.1.2006", "2/1/06", "2-1-06",
		"2 Jan 2006", "2 January 2006", "Jan 2 2006", "January 2 2006"}
	shortDates = []string{"2/1", "2-1", "2.1", "2 Jan", "2 January", "Jan 2", "January 2"}
	weekdays   = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
)

// parseDate understands ISO dates, day-first dates (15/11/2026, 15-11-26, 15/11), month names
// (15 Nov), "today", "tomorrow" and weekdays ("friday", "next friday" = the coming one). A date
// without a year that has already passed this year means next year, since answers are about what's
// coming.
func parseDate(s string, today time.Time) (string, error) {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, ",", "")))
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	day := func(t time.Time) (string, error) { return t.Format(time.DateOnly), nil }

	switch s {
	case "today":
		return day(today)
	case "tomorrow":
		return day(today.AddDate(0, 0, 1))
	}
	if i := slices.Index(weekdays, strings.TrimPrefix(s, "next ")); i >= 0 {
		ahead := (i - int(today.Weekday()) + 7) % 7
		if ahead == 0 {
			ahead = 7
		}
		return day(today.AddDate(0, 0, ahead))
	}
	for _, layout := range fullDates {
		if t, err := time.Parse(layout, s); err == nil {
			return day(t)
		}
	}
	for _, layout := range shortDates {
		if t, err := time.Parse(layout, s); err == nil {
			t = time.Date(today.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			if t.Before(today) {
				t = t.AddDate(1, 0, 0)
			}
			return day(t)
		}
	}
	return "", errors.New("I need a date, like 15/11/2026, 15 Nov or tomorrow")
}

// pickOption matches an answer to one of the options, ignoring case; "passed" matches "pass" and
// "f" matches "fail" when nothing else starts with it.
func pickOption(answer string, options []string) (string, error) {
	a := strings.ToLower(answer)
	var close []string
	for _, o := range options {
		lower := strings.ToLower(o)
		if a == lower {
			return o, nil
		}
		if strings.HasPrefix(a, lower) || strings.HasPrefix(lower, a) {
			close = append(close, o)
		}
	}
	if len(close) == 1 {
		return close[0], nil
	}
	return "", fmt.Errorf("please answer with one of: %s", strings.Join(options, ", "))
}

func parseYesNo(answer string) (bool, error) {
	switch strings.ToLower(strings.Trim(answer, " .!")) {
	case "yes", "y", "yeah", "yep", "true", "ok", "okay", "1":
		return true, nil
	case "no", "n", "nope", "false", "0":
		return false, nil
	}
	return false, errors.New("please answer yes or no")
}

// match finds the one choice an answer means: an exact name or id first, otherwise the only name
// that contains the answer. Several possible matches are listed so the person can be more exact.
func match(answer string, list []Choice, what string) (string, error) {
	a := strings.ToLower(answer)
	var exact, partial []Choice
	for _, c := range list {
		name, id := strings.ToLower(c.Name), strings.ToLower(c.ID)
		switch {
		case a == name || a == id:
			exact = append(exact, c)
		case len(a) >= 2 && (strings.Contains(name, a) || strings.Contains(id, a)):
			partial = append(partial, c)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0].ID, nil
	case len(exact) == 0 && len(partial) == 1:
		return partial[0].ID, nil
	case len(exact)+len(partial) == 0:
		return "", &NotInListError{What: what, Name: answer}
	}
	var names []string
	for _, c := range append(exact, partial...) {
		names = append(names, c.Name)
	}
	if len(names) > 5 {
		names = append(names[:5], "…")
	}
	return "", fmt.Errorf("which one do you mean: %s?", strings.Join(names, ", "))
}

// NotInListError means an answer names nothing in the list, e.g. a new entry; Seyal can offer to add it.
type NotInListError struct{ What, Name string }

func (e *NotInListError) Error() string {
	return fmt.Sprintf("I don't know a %s called %q", e.What, e.Name)
}

var listSeparator = regexp.MustCompile(`\s*(?:,|;|\n|\band\b)\s*`)

func splitList(s string) []string {
	var parts []string
	for _, p := range listSeparator.Split(s, -1) {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// Command recognises a short command at the start of a message, such as "submit" or "cancel
// because it is no longer needed", among the given words. It returns the command and the rest of the
// message as the reason. A word with underscores may be typed with spaces ("send to ops").
func Command(text string, words []string) (word, reason string, ok bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, w := range words {
		typed := w
		if spaced := strings.ReplaceAll(w, "_", " "); strings.HasPrefix(lower, spaced) {
			typed = spaced
		}
		if lower == typed {
			return w, "", true
		}
		if rest, found := strings.CutPrefix(lower, typed); found && (rest[0] == ' ' || rest[0] == ':') {
			reason = strings.TrimLeft(strings.TrimSpace(text)[len(typed):], " :-")
			for _, filler := range []string{"because ", "as ", "since "} {
				if strings.HasPrefix(strings.ToLower(reason), filler) {
					reason = reason[len(filler):]
				}
			}
			return w, strings.TrimSpace(reason), true
		}
	}
	return "", "", false
}
