// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import "testing"

// TestReadMoveWords: the three shapes of "move someone's open work" on Tell Seyal (D75, M3), and words
// that only look like them.
func TestReadMoveWords(t *testing.T) {
	tests := []struct{ text, from, to string }{
		// 1. <move word> [all [of]] <Name>'s <word> to <Name>
		{"Move Ben's work to Dev", "Ben", "Dev"},
		{"move ben's work to dev.", "ben", "dev"},
		{"Give all of Ben’s jobs to Dev", "Ben", "Dev"}, // any one word, and a curly apostrophe
		{"Pass all Ben Example's items to Dev Example", "Ben Example", "Dev Example"},
		{"Hand  Mary O'Neil's   work to Dev", "Mary O'Neil", "Dev"},
		// 2. <Name> is <anything>, <move word> [all [of]] his|her|their <word> to <Name>
		{"Ben is on leave Thu and Fri, move his work to Dev", "Ben", "Dev"},
		{"Asha is away, give her items to Chitra.", "Asha", "Chitra"},
		{"Sam is unwell today, hand all of their work to Dev", "Sam", "Dev"},
		// 3. <move word> [all [of]] [the] <word> from <Name> to <Name>
		{"Move the work from Ben to Dev", "Ben", "Dev"},
		{"move all of the items from Ben Example to Dev", "Ben Example", "Dev"},
		{"Give work from Ben to Dev.", "Ben", "Dev"},
		// Small words after the receiver's name aren't part of it.
		{"Move Ben's work to Dev please", "Ben", "Dev"},
		{"Move Ben's work to Dev!", "Ben", "Dev"},
		{"Move Ben's work to Dev, thanks", "Ben", "Dev"},
		{"Move Ben's work to Dev Example; thank you.", "Ben", "Dev Example"},
		{"Move Ben's work to Dev Example thank you", "Ben", "Dev Example"},
	}
	for _, tt := range tests {
		from, to, ok := readMoveWords(tt.text)
		if !ok || from != tt.from || to != tt.to {
			t.Errorf("readMoveWords(%q) = %q, %q, %v; want %q, %q", tt.text, from, to, ok, tt.from, tt.to)
		}
	}

	for _, text := range []string{
		"Move Ben's work to Dev?", // a question isn't a command
		"Ben is on leave, can you move his work to Dev?",
		"Hand the Sundaram documents to Dev, Ben collected them", // no possessive and no "from": new work
		"Ask Ben to give the March file to Dev",                  // doesn't start with a move word or a name
		"Move Ben's work",                                        // nobody to give it to
		"Ben is on leave, move Asha's work to Dev",               // shape 2 needs his, her or their
		"Take Ben's work to Dev",                                 // not a move word
		"Move the March file from the shelf",
		"Pass to Dev, he knows them", // a one-item hand-over in a thread (D73), not this
		"",
	} {
		if from, to, ok := readMoveWords(text); ok {
			t.Errorf("readMoveWords(%q) = %q, %q; want no move", text, from, to)
		}
	}
}
