// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// The lock-screen sentences of the design's table (section 2.1, "On the lock screen").
func TestText(t *testing.T) {
	w := Words{Number: "J-2026-0007", From: "Ben", Team: "Ops"}
	tests := []struct {
		kind string
		w    Words
		want string
	}{
		{NewWork, w, "New work from Ben: J-2026-0007"},
		{NewWork, Words{Number: "J-2026-0007"}, "New work from Seyal: J-2026-0007"},
		{TeamWork, w, "New work for Ops from Ben: J-2026-0007"},
		{Approve, w, "Your approval is needed: J-2026-0007"},
		{ApproveAgain, w, "Please approve again (it changed): J-2026-0007"},
		{Reminder, w, "Reminder: J-2026-0007 is waiting on you"},
		{NoEscalate, w, "Seyal couldn't escalate J-2026-0007"},
		// Decision 2: the business's name only when the caller gives it (two or more businesses).
		{NewWork, Words{Business: "Kaveri Associates (demo)", Number: "J-2026-0007", From: "Ben"},
			"Kaveri Associates (demo) · New work from Ben: J-2026-0007"},
		{"item.shared", w, ""}, // a kind Text doesn't know says nothing
	}
	for _, tt := range tests {
		if got := Text(tt.kind, tt.w); got != tt.want {
			t.Errorf("Text(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
	if Title != "Seyal" {
		t.Errorf("Title = %q, want Seyal", Title)
	}
}

func TestGroupedTexts(t *testing.T) {
	tests := []struct{ got, want string }{
		{Several(3, ""), "3 new things for you"},
		{Several(1, ""), "1 new thing for you"},
		{Moved(12, "Chitra", ""), "12 items moved to you by Chitra"},
		{Moved(1, "Chitra", "Kaveri Associates (demo)"), "Kaveri Associates (demo) · 1 item moved to you by Chitra"},
		{Quiet(map[string]int{Reminder: 2, NewWork: 1}, ""), "While it was quiet: 2 reminders, 1 new work"},
		{Quiet(map[string]int{NewWork: 1, TeamWork: 2, Approve: 1, ApproveAgain: 1, NoEscalate: 1, Reminder: 1}, ""),
			"While it was quiet: 1 reminder, 3 new work, 2 approvals needed, 1 couldn't be escalated"},
		{Quiet(map[string]int{Approve: 1}, "Kaveri Associates (demo)"), "Kaveri Associates (demo) · While it was quiet: 1 approval needed"},
		{Quiet(nil, ""), ""},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

// A name is a person's own words: it can't break the line, turn the text around, or make the note big.
func TestNamesAreCleaned(t *testing.T) {
	got := Text(NewWork, Words{Number: "J-2026-0007\n", From: "Ben\nCall me\t now"})
	if got != "New work from Ben Call me now: J-2026-0007" {
		t.Errorf("line breaks: %q", got)
	}
	if got := Text(NewWork, Words{Number: "J-2026-0007", From: "‮neB"}); got != "New work from neB: J-2026-0007" {
		t.Errorf("a right-to-left mark is kept: %q", got)
	}
	long := strings.Repeat("அ", 100) // Tamil letters: three bytes each
	got = Text(TeamWork, Words{Business: long, Number: strings.Repeat("9", 100), From: long, Team: long})
	if n := utf8.RuneCountInString(got); n > 4*maxName+maxNumber+40 {
		t.Errorf("a long name makes a %d-letter note: %q", n, got)
	}
	if !strings.Contains(got, "…") || !utf8.ValidString(got) {
		t.Errorf("a long name isn't cut cleanly: %q", got)
	}
	// A letter can be up to 4 bytes (an emoji), so the bytes are capped too (review fix of 4a-3): the
	// longest sentence stays at 361 bytes, leaving room for the rest of the payload under the design's
	// 500 bytes.
	const most = 3*maxNameBytes + maxNumberBytes + len(" · New work for  from : ")
	for _, letter := range []string{"அ", "😀", "a"} {
		long := strings.Repeat(letter, 100)
		got := Text(TeamWork, Words{Business: long, Number: strings.Repeat(letter, 100), From: long, Team: long})
		if len(got) > most || !utf8.ValidString(got) || !strings.Contains(got, "…") {
			t.Errorf("%s: the note is %d bytes (most %d): %q", letter, len(got), most, got)
		}
	}
	if most > 400 {
		t.Errorf("the longest note can be %d bytes; keep it well under the design's 500", most)
	}
	// An ordinary long name is not cut.
	if forty := strings.Repeat("b", maxName); Text(NewWork, Words{Number: "J-2026-0007", From: forty}) != "New work from "+forty+": J-2026-0007" {
		t.Error("a 40-letter name is cut")
	}
}

// Words has room only for what a lock screen may show. Adding a field here (a title, a note) needs a
// decision first (D85, decision 1), so this test names the fields on purpose.
func TestWordsHoldOnlyWhatMayShow(t *testing.T) {
	var fields []string
	words := reflect.TypeFor[Words]()
	for i := range words.NumField() {
		fields = append(fields, words.Field(i).Name)
	}
	if want := []string{"Business", "Number", "From", "Team"}; !slices.Equal(fields, want) {
		t.Errorf("Words has %v, want only %v", fields, want)
	}
}
