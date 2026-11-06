// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"regexp"
	"strings"
)

// Words on Tell Seyal that move someone's open work to someone else (D75, M3): "Move Ben's work to
// Dev", "Ben is on leave Thu and Fri, move his work to Dev", "Move the work from Ben to Dev". This
// file only reads the words; move.go finds the people and the items.
//
// Only the whole message counts, in one of three fixed shapes (a full stop at the end is fine). The
// word after the name ("work", "items", or a word a business uses) is any one word, so the engine
// needn't know it. Anything else goes to the usual Tell Seyal routing, so "Hand the Sundaram documents
// to Dev, Ben collected them" still starts new work. A message ending in "?" is a question.

// moveVerb is a word that starts a move: move, give, pass or hand.
const moveVerb = `(?:move|give|pass|hand)`

// moveShapes are the three shapes, each with two groups: the person whose work it is, and who gets it.
var moveShapes = []*regexp.Regexp{
	// 1. Move [all [of]] Ben's work to Dev
	regexp.MustCompile(`^` + moveVerb + ` (?:all (?:of )?)?(.+?)'s [^ ]+ to (.+)$`),
	// 2. Ben is on leave Thu and Fri, move [all [of]] his|her|their work to Dev
	regexp.MustCompile(`^(.+?) is .+, ` + moveVerb + ` (?:all (?:of )?)?(?:his|her|their) [^ ]+ to (.+)$`),
	// 3. Move [all [of]] [the] work from Ben to Dev
	regexp.MustCompile(`^` + moveVerb + ` (?:all (?:of )?)?(?:the )?[^ ]+ from (.+?) to (.+)$`),
}

// readMoveWords reads text as a "move someone's work" command. from and to are the words that name
// the two people, as typed; ok is false when the text has none of the shapes.
func readMoveWords(text string) (from, to string, ok bool) {
	text = strings.TrimSuffix(strings.Join(strings.Fields(strings.ReplaceAll(text, "’", "'")), " "), ".")
	if strings.HasSuffix(text, "?") {
		return "", "", false // a question, not a command
	}
	lower := strings.ToLower(text)
	if len(lower) != len(text) { // a letter whose small form is longer or shorter: keep the cuts in step
		text = lower
	}
	for _, shape := range moveShapes {
		if m := shape.FindStringSubmatchIndex(lower); m != nil {
			from, to = strings.TrimSpace(text[m[2]:m[3]]), receiver(text[m[4]:m[5]])
			return from, to, from != "" && to != ""
		}
	}
	return "", "", false
}

// afterName are polite words after the receiver's name that aren't part of it: "to Dev please".
var afterName = []string{"please", "thanks", "thank you"}

// receiver is the receiver's name from the words after "to": what comes before a comma, ";" or "!",
// without a full stop or a polite word at the end. "Dev, thanks" and "Dev please" are both "Dev".
func receiver(words string) string {
	if i := strings.IndexAny(words, ",;!"); i >= 0 {
		words = words[:i]
	}
	words = strings.TrimSpace(strings.TrimRight(words, ". "))
	for _, w := range afterName {
		if n := len(words) - len(w); n > 0 && words[n-1] == ' ' && strings.EqualFold(words[n:], w) {
			return strings.TrimSpace(words[:n])
		}
	}
	return words
}
