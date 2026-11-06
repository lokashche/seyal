// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"testing"
)

// TestReadHandWords: the hand-over words, the name and the note after a separator (D68, section 6).
func TestReadHandWords(t *testing.T) {
	for _, tt := range []struct {
		text string
		want handWords
	}{
		{"pass to Dev", handWords{Do: doPass, To: "Dev"}},
		{"Pass to Dev.", handWords{Do: doPass, To: "Dev"}},
		{"pass to Dev, he knows Sundaram well", handWords{Do: doPass, To: "Dev", Note: "he knows Sundaram well", Sep: true}},
		{"pass on to Ben: urgent", handWords{Do: doPass, To: "Ben", Note: "urgent", Sep: true}},
		{"give to Ben - the March file", handWords{Do: doPass, To: "Ben", Note: "the March file", Sep: true}},
		{"hand to Ben because he is free", handWords{Do: doPass, To: "Ben", Note: "he is free", Sep: true}},
		{"assign to Ben Example", handWords{Do: doPass, To: "Ben Example"}},
		{"give it to  Dev ,  he knows them", handWords{Do: doPass, To: "Dev", Note: "he knows them", Sep: true}},
		{"pass to Mary-Jane", handWords{Do: doPass, To: "Mary-Jane"}},
		{"pass to Dev;", handWords{Do: doPass, To: "Dev"}},
		{"pass to Dev, can you check the March file?", handWords{Do: doPass, To: "Dev", Note: "can you check the March file?", Sep: true}},
		{"pass to the ops team", handWords{Do: doPass, To: "the ops team"}},
		{"give back to the team", handWords{Do: doPass, To: "the team"}},
		{"I'll take it", handWords{Do: doTake}},
		{"I’ll take it!", handWords{Do: doTake}},
		{"i will take it", handWords{Do: doTake}},
		{"Take it.", handWords{Do: doTake}},
		{"send back to Asha because the statement is missing", handWords{Do: doSendBack, To: "Asha", Note: "the statement is missing", Sep: true}},
		{"send back to Asha: statement missing", handWords{Do: doSendBack, To: "Asha", Note: "statement missing", Sep: true}},
		{"send it back to draft because the fee is wrong", handWords{Do: doSendBack, To: "draft", Note: "the fee is wrong", Sep: true}},
		// The name runs to the end when there is no separator; finding the person decides if it is whole.
		{"give to Ben the March file when it comes", handWords{Do: doPass, To: "Ben the March file when it comes"}},
	} {
		got, ok := readHandWords(tt.text, nil)
		if !ok || got != tt.want {
			t.Errorf("readHandWords(%q) = %+v, %v; want %+v", tt.text, got, ok, tt.want)
		}
	}
}

// TestEverydaySentencesAreNotHandOvers: ordinary sentences never read as hand-over words.
func TestEverydaySentencesAreNotHandOvers(t *testing.T) {
	for _, text := range []string{
		"take it to the bank tomorrow",
		"I'll take it from here once the client calls",
		"mine",
		"pass the file to Dev",
		"can you pass to Dev?",
		"pass to Dev?", // a question, not a command
		"give it to Ben?",
		"Ben will pass to Dev later",
		"please send the statement back to Asha",
		"passed to Dev yesterday",
		"pass to",
		"give to ",
		"handover to Dev",
		"take",
	} {
		if h, ok := readHandWords(text, nil); ok {
			t.Errorf("readHandWords(%q) = %+v; want not a hand-over", text, h)
		}
	}
}

// TestProcessWordWins: a stored process with a move named like one of Seyal's words keeps it (D68,
// decision 24): "take" presses the process's move, so "take it" isn't Seyal's take.
func TestProcessWordWins(t *testing.T) {
	for _, tt := range []struct {
		moves []string
		text  string
		ok    bool
	}{
		{[]string{"take"}, "take it", false},
		{[]string{"take"}, "I'll take it", true}, // doesn't start with the process's word
		{[]string{"pass"}, "pass to Dev", false},
		{[]string{"pass"}, "pass on to Dev", false},
		{[]string{"pass_on"}, "pass on to Dev", false},
		{[]string{"pass_on"}, "pass to Dev", true},
		{[]string{"send_back"}, "send back to Asha because x", false},
		{[]string{"send", "send_to_ops"}, "send back to Asha because x", true}, // not a word Seyal keeps for itself
		{[]string{"give_back"}, "give back to the team", false},
		{[]string{"give_back"}, "give to Dev", true},
	} {
		if _, ok := readHandWords(tt.text, tt.moves); ok != tt.ok {
			t.Errorf("readHandWords(%q, moves %v) ok = %v, want %v", tt.text, tt.moves, ok, tt.ok)
		}
	}
}

// TestWhoIsNamed: the whole first name or the whole full name, in any case, and nothing else
// (decision 19).
func TestWhoIsNamed(t *testing.T) {
	people := []Choice{{"1", "Ben Example"}, {"2", "Ben Sample"}, {"3", "Dev Example"}, {"4", "Debra Test"}}
	for _, tt := range []struct {
		words string
		want  []string
	}{
		{"Dev", []string{"3"}},
		{"dev", []string{"3"}},
		{"DEV EXAMPLE", []string{"3"}},
		{"Ben", []string{"1", "2"}},
		{"Ben Sample", []string{"2"}},
		{"Deb", nil},        // no part of a name
		{"Deva", nil},       // no near-spellings
		{"Example", nil},    // a family name alone is not the first name
		{"Dev Exampl", nil}, // no part of a full name
		{"Ben the March file", nil},
		{"", nil},
	} {
		var got []string
		for _, c := range whoIsNamed(tt.words, people) {
			got = append(got, c.ID)
		}
		if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) || (len(got) > 1 && got[1] != tt.want[1]) {
			t.Errorf("whoIsNamed(%q) = %v, want %v", tt.words, got, tt.want)
		}
	}
	if !startsWithName("Ben the March file when it comes", people) || startsWithName("Ben", people) ||
		startsWithName("the courier tomorrow", people) {
		t.Error("startsWithName: want true only for a sentence that starts with a name and goes on")
	}
}

func TestTeamWordsAndMaybeNames(t *testing.T) {
	for _, tt := range []struct {
		words, name string
		ok          bool
	}{
		{"the team", "", true}, {"team", "", true}, {"the ops team", "ops", true}, {"Operations team", "operations", true},
		{"Dev", "", false}, {"the courier", "", false},
	} {
		if name, ok := teamWords(tt.words); name != tt.name || ok != tt.ok {
			t.Errorf("teamWords(%q) = %q, %v; want %q, %v", tt.words, name, ok, tt.name, tt.ok)
		}
	}
	for words, want := range map[string]bool{
		"Deb": true, "Deb Kumar": true, "him": false, "me": false, "the courier": false,
		"charity the old laptops and chairs": false,
	} {
		if mayBeName(words) != want {
			t.Errorf("mayBeName(%q) = %v, want %v", words, !want, want)
		}
	}
}

// TestNamesSomeone: Seyal's replies that name a person are found, so they can be kept from the model.
func TestNamesSomeone(t *testing.T) {
	names := []string{"Dev Example", "Asha Test", "K. Ramesh", "A.K. Raman", "Dr. Meena Example", "S.Priya", "Dr. K"}
	for text, want := range map[string]bool{
		"Passed to Dev Example.":                         true,
		"Sent back to Asha Test (draft).":                true,
		"It can go to: Dev Example, or the Ops team.":    true,
		"Passed to K. Ramesh.":                           true,
		"Passed to A.K. Raman.":                          true,
		"Sent back to Dr. Meena Example (draft).":        true,
		"Which one do you mean: S.Priya, or Dev?":        true,
		"Passed to Dr. K.":                               true, // only a title and an initial: the whole name counts
		"Which client is this for?":                      false,
		"Everything needed is filled in. Say \"submit\"": false,
		"The developer will look at it.":                 false, // "developer" isn't the word "dev"
		"Dr. Who has a K in it.":                         false, // a title or an initial alone isn't a name
	} {
		if got := namesSomeone(text, names); got != want {
			t.Errorf("namesSomeone(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestPicked: a reply to "which one?" picks a button by its own words, by the words on it typed out,
// or by a first word only one button has; anything else picks nothing.
func TestPicked(t *testing.T) {
	options := []Quick{
		{Label: "Ben", Text: "pass to Ben, he is free", Person: "1"},
		{Label: "Ben Kumar", Text: "pass to Ben Kumar, he is free", Person: "2"},
	}
	for text, want := range map[string]string{
		"pass to Ben, he is free":       "1", // the button "Ben", although "Ben" is also Ben Kumar's first name
		"pass to Ben Kumar, he is free": "2",
		"Ben":                           "1",
		"ben kumar.":                    "2",
		"Kumar":                         "",
		"Bank statements":               "",
	} {
		q, ok := picked(options, text)
		if ok != (want != "") || q.Person != want {
			t.Errorf("picked(%q) = %+v, %v; want person %q", text, q, ok, want)
		}
	}
	steps := []Quick{{Label: "draft (Asha)", Move: "back_to_sales"}, {Label: "study (Dev)", Move: "back_to_study"}}
	if q, ok := picked(steps, "draft"); !ok || q.Move != "back_to_sales" {
		t.Errorf("picked(draft) = %+v, %v; want the move to draft", q, ok)
	}
}
