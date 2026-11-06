// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"slices"
	"strings"

	"seyal/internal/playbook"
)

// Hand-over words typed in an item's thread (D68, section 6; build part 5): "pass to Dev, he knows
// them", "give back to the team", "I'll take it", "send back to Asha because …". This file only reads
// the words; handover.go finds the people and does the hand-over.
//
// A typed hand-over runs at once only when the message is the whole command (decision 20): the
// hand-over words, then a name, then nothing, or a separator (a comma, a colon, a dash or "because")
// and a note. Anything else is an ordinary sentence and never moves work.

// handWords is a hand-over typed in a thread, as read from the words alone.
type handWords struct {
	Do   string // doPass (also "give back to the team"), doTake or doSendBack
	To   string // the words after "to", up to the separator: a name, a team, or a step
	Note string // the words after the separator: the note, or the reason
	Sep  bool   // a separator was typed
}

const (
	doPass     = "pass"
	doTake     = "take"
	doSendBack = "send_back"
)

// passWords start a pass on. Longer forms come first, so "pass on to" isn't read as "pass" + "on to".
var passWords = []string{
	"pass it on to", "pass on to", "pass it to", "pass to",
	"give it back to", "give back to", "give it to", "give to",
	"hand it over to", "hand over to", "hand it to", "hand to",
	"assign it to", "assign to",
}

// sendBackWords start a send-back to a person or a step.
var sendBackWords = []string{"send it back to", "send back to"}

// takeWords take the item, but only as the whole message: "take it to the bank tomorrow" is a note.
var takeWords = []string{"take it", "i'll take it", "i will take it", "ill take it"}

// separators end the name and start the note. A dash needs a space before it, so a name like
// "Mary-Jane" stays whole.
var separators = []string{",", ":", " - ", " – ", " — ", " because "}

// readHandWords reads text as a hand-over. ok is false when the text doesn't start with hand-over
// words, or when the process has a move of the same name (playbook.ReservedMoves): a process's own
// word wins (decision 24), as it did before these words existed. moves are the process's move names.
func readHandWords(text string, moves []string) (h handWords, ok bool) {
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "’", "'")), " ")
	lower := strings.ToLower(text)
	if len(lower) != len(text) { // a letter whose small form is longer or shorter: keep the cuts in step
		text = lower
	}
	if t := strings.TrimRight(lower, " .!"); slices.Contains(takeWords, t) {
		return handWords{Do: doTake}, !processHas(moves, t)
	}
	for _, list := range []struct {
		do    string
		words []string
	}{{doPass, passWords}, {doSendBack, sendBackWords}} {
		for _, w := range list.words {
			if !strings.HasPrefix(lower, w+" ") {
				continue
			}
			if processHas(moves, w) {
				return handWords{}, false
			}
			h = handWords{Do: list.do, To: text[len(w)+1:]}
			cut, end := len(h.To), len(h.To)
			for _, sep := range separators {
				if i := strings.Index(strings.ToLower(h.To), sep); i >= 0 && i < cut {
					cut, end = i, i+len(sep)
				}
			}
			if cut < len(h.To) {
				h.Sep, h.Note = true, strings.TrimSpace(h.To[end:])
			}
			h.To = strings.TrimSpace(h.To[:cut])
			if !h.Sep && strings.HasSuffix(h.To, "?") {
				return handWords{}, false // "pass to Dev?" is a question, read as usual
			}
			h.To = strings.TrimRight(h.To, " .!;")
			return h, h.To != ""
		}
	}
	return handWords{}, false
}

// processHas reports whether the process has a move named with one of Seyal's own chat words that the
// typed words start with ("pass" for "pass to", "take" for "take it").
func processHas(moves []string, typed string) bool {
	for _, m := range moves {
		spaced := strings.ReplaceAll(m, "_", " ")
		if slices.Contains(playbook.ReservedMoves, m) && strings.HasPrefix(typed+" ", spaced+" ") {
			return true
		}
	}
	return false
}

// whoIsNamed returns the people a typed name means: the whole first name or the whole full name, in any
// upper or lower case. No part of a name and no near-spellings (decision 19).
func whoIsNamed(words string, people []Choice) []Choice {
	words = strings.ToLower(strings.Join(strings.Fields(words), " "))
	var out []Choice
	for _, p := range people {
		full := strings.ToLower(strings.Join(strings.Fields(p.Name), " "))
		first, _, _ := strings.Cut(full, " ")
		if words != "" && (words == full || words == first) {
			out = append(out, p)
		}
	}
	return out
}

// startsWithName reports whether words begin with someone's whole first or full name and go on after
// it: "Ben the March file when it comes". Such a message is a sentence, not a whole command.
func startsWithName(words string, people []Choice) bool {
	fields := strings.Fields(words)
	for n := 1; n < len(fields); n++ {
		if len(whoIsNamed(strings.Join(fields[:n], " "), people)) > 0 {
			return true
		}
	}
	return false
}

// teamWords reports whether words name a team: "team" or "the team" (name ""), or "the Ops team" and
// "ops team" (name "Ops").
func teamWords(words string) (name string, ok bool) {
	fields := strings.Fields(strings.ToLower(words))
	if len(fields) == 0 || fields[len(fields)-1] != "team" {
		return "", false
	}
	fields = fields[:len(fields)-1]
	if len(fields) > 0 && fields[0] == "the" {
		fields = fields[1:]
	}
	return strings.Join(fields, " "), true
}

// notNames are words after "to" that are never a person's name, so "give to him" is a note.
var notNames = []string{"me", "myself", "him", "her", "them", "us", "you", "someone", "somebody", "anyone",
	"everyone", "it", "this", "that", "whoever"}

// mayBeName reports whether words could be a name Seyal doesn't know ("Deb", "Deb Kumar"): one to three
// words, not a pronoun, and not starting with "the". Then Seyal says it doesn't know them; longer words
// are an ordinary sentence.
func mayBeName(words string) bool {
	fields := strings.Fields(strings.ToLower(words))
	return len(fields) >= 1 && len(fields) <= 3 && fields[0] != "the" && !slices.Contains(notNames, fields[0])
}

// namesSomeone reports whether text names any of the names (people's names, also ones they had
// before): their whole run of words, or their first word that is a real name, as a word of its own.
// "A.K. Raman" and "Dr. Meena Example" are read as words too, so "raman" and "meena" count, and an
// initial or a title alone doesn't. Seyal's replies that do are left out of what the AI model sees
// (D68, decision 18).
func namesSomeone(text string, names []string) bool {
	words := nameWords(text)
	for _, name := range names {
		nw := nameWords(name)
		if len(nw) == 0 {
			continue
		}
		for i := range words {
			if i+len(nw) <= len(words) && slices.Equal(words[i:i+len(nw)], nw) {
				return true
			}
		}
		for _, w := range nw {
			if len([]rune(w)) < 2 || slices.Contains(titles, w) {
				continue
			}
			if slices.Contains(words, w) {
				return true // the first real name is enough; the full name has it
			}
			break
		}
	}
	return false
}

// titles come before a name and are not a name themselves.
var titles = []string{"dr", "mr", "mrs", "ms", "miss", "prof", "sri", "shri", "smt"}

// nameWords breaks text into words in small letters: letters, digits, '-' and an apostrophe; anything
// else (a space, '.', ',', ':', a bracket) ends a word.
func nameWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 || r == '-' || r == '\'')
	})
}
