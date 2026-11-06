// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Title is the title of every phone note. The lock screen is not private, so the title never names
// the business or the item.
const Title = "Seyal"

// Words are the only things a phone note may say (decision 1): the item's number, a colleague's or a
// team's name, and the business's name. There is no place here for a title, a hand-over note, a field
// value or a sentence from the thread, so Text can't put one on a lock screen.
type Words struct {
	// Business is the business's name. Give it only when the person's login belongs to two or more
	// businesses (decision 2); "" leaves it out.
	Business string
	Number   string // e.g. J-2026-0007
	From     string // the colleague who did it; "" when Seyal did it on its own
	Team     string // the team's name, for TeamWork
}

// How long each piece may be, in letters and in bytes. Names and numbers are short; these limits only
// stop a very long name from making a phone note too big. A letter can take up to 4 bytes (an emoji),
// so a letter limit alone isn't enough: the byte limits keep the longest sentence (a team note with
// the business's name) at 3×96 + 48 + 25 = 361 bytes, leaving room for the rest of the payload under
// the design's 500 bytes.
const (
	maxName        = 40
	maxNameBytes   = 96
	maxNumber      = 30
	maxNumberBytes = 48
)

// Text is the body of a phone note of one kind (see the kinds in notify.go), or "" for a kind it
// doesn't know.
func Text(kind string, w Words) string {
	number, from, team := clean(w.Number, maxNumber, maxNumberBytes), cleanName(w.From), cleanName(w.Team)
	if from == "" {
		from = "Seyal"
	}
	var body string
	switch kind {
	case NewWork:
		body = fmt.Sprintf("New work from %s: %s", from, number)
	case TeamWork:
		body = fmt.Sprintf("New work for %s from %s: %s", team, from, number)
	case Approve:
		body = "Your approval is needed: " + number
	case ApproveAgain:
		body = "Please approve again (it changed): " + number
	case Reminder:
		body = fmt.Sprintf("Reminder: %s is waiting on you", number)
	case NoEscalate:
		body = "Seyal couldn't escalate " + number
	default:
		return ""
	}
	return withBusiness(w.Business, body)
}

// Several is one phone note for n notes due for one person at once: "3 new things for you".
func Several(n int, business string) string {
	return withBusiness(business, fmt.Sprintf("%d new %s for you", n, plural(n, "thing", "things")))
}

// Moved is one phone note for an admin's bulk move to the person: "12 items moved to you by Chitra".
func Moved(n int, by, business string) string {
	by = cleanName(by)
	if by == "" {
		by = "Seyal"
	}
	return withBusiness(business, fmt.Sprintf("%d %s moved to you by %s", n, plural(n, "item", "items"), by))
}

// Quiet is the morning note for what waited through quiet hours, counted by kind: "While it was
// quiet: 2 reminders, 1 new work". Kinds with no count are left out; "" when nothing waited. (When
// only one note waited, the reader sends that note's own Text instead.)
func Quiet(counts map[string]int, business string) string {
	var parts []string
	add := func(n int, one, many string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, one, many)))
		}
	}
	add(counts[Reminder], "reminder", "reminders")
	add(counts[NewWork]+counts[TeamWork], "new work", "new work")
	add(counts[Approve]+counts[ApproveAgain], "approval needed", "approvals needed")
	add(counts[NoEscalate], "couldn't be escalated", "couldn't be escalated")
	if len(parts) == 0 {
		return ""
	}
	return withBusiness(business, "While it was quiet: "+strings.Join(parts, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// withBusiness puts the business's name in front, when there is one:
// "Kaveri Associates (demo) · New work from Ben: J-2026-0007".
func withBusiness(business, body string) string {
	if b := cleanName(business); b != "" {
		return b + " · " + body
	}
	return body
}

// cleanName cleans a person's, a team's or the business's name (see clean).
func cleanName(s string) string { return clean(s, maxName, maxNameBytes) }

// clean makes a name or a number safe for one line: line breaks, tabs, other control characters and
// the marks that change the direction of text (which could make a note read backwards) become spaces,
// and runs of spaces become one. A piece longer than letters letters or bytes bytes is cut, ending in
// "…", so it keeps at most letters letters and bytes bytes, "…" included.
func clean(s string, letters, bytes int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == utf8.RuneError
	}), " ")
	if utf8.RuneCountInString(s) <= letters && len(s) <= bytes {
		return s
	}
	var b strings.Builder
	kept := 0
	for _, r := range s {
		if kept == letters-1 || b.Len()+utf8.RuneLen(r)+len("…") > bytes {
			break
		}
		b.WriteRune(r)
		kept++
	}
	return strings.TrimSpace(b.String()) + "…"
}
