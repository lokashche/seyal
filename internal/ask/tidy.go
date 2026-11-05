// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"seyal/internal/ai"
)

// Tidying the words of a voice note (D66). Speech services write down everything, "umm"s and false
// starts included. A chat model (SEYAL_AI_TIDY) cleans that up before the person sees the words. It
// must keep the meaning: Seyal checks its answer and uses the words as heard when the answer adds
// a number or a name.

// tidyTimeout is how long the person waits for the tidy-up of a short note at most; after that the
// words as heard are used. Tidying is quick, and the person is waiting with the recording just made.
// The model writes the whole note back, so a longer note gets a second more for every 100 letters
// (tidyTime).
var tidyTimeout = 8 * time.Second

// tidyMaxLetters is the longest note that is tidied: 2000 letters (about 3 minutes of English, less of
// Tamil, whose letters take several tokens each). A longer note would take the model longer than
// the person should wait, and a call given up on is still paid for, so the words as heard are used
// without asking.
const tidyMaxLetters = 2000

// tidyTime is how long the tidy-up of a note of n letters may take: up to 28 seconds at the most.
func tidyTime(n int) time.Duration { return tidyTimeout + time.Duration(n/100)*time.Second }

const tidyRules = `You tidy the words of a voice note, written down by a speech service, before the speaker checks them.
- Remove filler sounds and words (umm, uh, ah, er, hmm, and Tamil fillers such as "அது வந்து" or "ம்ம்" when they are only filler).
- Remove false starts and words said twice by mistake.
- Fix punctuation, and the capital letter at the start of a sentence.
- Keep the speaker's own words and their mix of languages exactly: Tamil, English, or Tamil written in English letters (Tanglish). Never translate, never change the script.
- Never add, remove or change facts, names, numbers, amounts, dates or times. Write numbers exactly as heard (do not turn words into digits or digits into words).
- If you are unsure, give the words back unchanged.
Answer only with JSON that follows the given schema: {"text": "<the tidied words>"}.`

var tidySchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"text": map[string]any{"type": "string"}},
	"required":             []string{"text"},
	"additionalProperties": false,
}

// Tidy asks the tidy-up model to clean the words heard in a voice note. It returns the tidied words,
// or "" when they can't be used (AI off for tidying, the note is too long to tidy, the model failed
// or was too slow, or its answer changed a number or a name): the caller then uses the words as
// heard. call is the model call to record in AI usage (nil when no call was made); a refused answer
// is recorded as a failed call, with the reason. It doesn't touch the database, so no transaction waits on the model.
func Tidy(ctx context.Context, setup *ai.Setup, raw string, link ai.Link) (tidied string, call *ai.Called) {
	raw = strings.TrimSpace(raw)
	n := utf8.RuneCountInString(raw)
	if raw == "" || n > tidyMaxLetters {
		return "", nil
	}
	maxTokens := 200 + 3*n // Tamil letters can take several tokens each
	resp, called, err := setup.Ask(ctx, ai.Tidy, ai.Request{Instructions: tidyRules, Input: raw, Schema: tidySchema,
		MaxTokens: maxTokens, Timeout: tidyTime(n)}, link)
	if errors.Is(err, ai.ErrOff) {
		return "", nil
	}
	if err != nil {
		return "", &called
	}
	var answer struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Text)), &answer); err != nil {
		called.Err = errTidyUnreadable // not the JSON error: it can quote the answer
		return "", &called
	}
	tidied = strings.TrimSpace(answer.Text)
	if err := keepsFacts(raw, tidied); err != nil {
		called.Err = err // shown in AI usage as a failed call, with the reason
		return "", &called
	}
	return tidied, &called
}

// Why a tidy-up's answer is refused. The reason goes into AI usage (ai_calls.error), which is kept
// for good, also when the person discards the voice note. So a reason never quotes a word of the note
// or of the answer: an amount or a name from a private note would stay in the database (D66).
var (
	errTidyEmpty      = errors.New("tidy-up refused: the answer is empty")
	errTidyLonger     = errors.New("tidy-up refused: the answer is longer than what was said")
	errTidyNumber     = errors.New("tidy-up refused: it has a number that wasn't said")
	errTidyName       = errors.New("tidy-up refused: it has a name that wasn't said")
	errTidyMoved      = errors.New("tidy-up refused: it moved or repeated a number, a name or a \"not\"")
	errTidyLeftOut    = errors.New("tidy-up refused: it left out a number, a name or a \"not\"")
	errTidyUnreadable = errors.New("tidy-up refused: the answer can't be read")
)

// keepsFacts checks the tidy-up's answer against the words heard. It refuses an answer that is
// empty or much longer, or that has a number or a capitalised word (a name, usually) that wasn't
// said. Words are compared ignoring capitals, since the tidy-up may start a sentence with one.
//
// It also checks that the facts are kept as said: the numbers, the names (a capital that doesn't
// start a sentence, in either text) and the words that say "not". In the answer they must come in
// the same order and as many times as heard, and none may be left out; only a fact said twice in a
// row (a false start, such as "Acme Traders, ah, Acme Traders") may be said once. So an answer that
// swaps two amounts, drops one, or drops a "not", is refused.
func keepsFacts(raw, tidied string) error {
	switch n := utf8.RuneCountInString(raw); {
	case tidied == "":
		return errTidyEmpty
	case utf8.RuneCountInString(tidied) > n+n/5+20:
		return errTidyLonger
	}
	said, answer := sentenceWords(raw), sentenceWords(tidied)
	heard := make([]string, len(said))
	for i, w := range said {
		heard[i] = strings.ToLower(w.text)
	}
	for _, w := range answer {
		r, _ := utf8.DecodeRuneInString(w.text)
		switch {
		case slices.Contains(heard, strings.ToLower(w.text)):
		case strings.ContainsFunc(w.text, unicode.IsDigit):
			return errTidyNumber
		case unicode.IsUpper(r):
			return errTidyName
		}
	}

	fact := map[string]bool{}
	for _, w := range slices.Concat(said, answer) {
		if w.fact() {
			fact[strings.ToLower(w.text)] = true
		}
	}
	facts := func(ws []sentenceWord) []string {
		var out []string
		for _, w := range ws {
			if lower := strings.ToLower(w.text); fact[lower] {
				out = append(out, lower)
			}
		}
		return out
	}
	before, after := facts(said), facts(answer)
	// No fact more often than said, nor out of order: after is before with some facts taken out.
	j := 0
	for _, f := range after {
		for j < len(before) && before[j] != f {
			j++
		}
		if j == len(before) {
			return errTidyMoved
		}
		j++
	}
	// Only repeats taken out: counting what was said twice in a row once, the facts are the same.
	if !slices.Equal(once(before), once(after)) {
		return errTidyLeftOut
	}
	return nil
}

// once takes out what is said twice in a row (one to four words): "acme traders acme traders 500"
// becomes "acme traders 500".
func once(ws []string) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w)
		for l := 1; l <= 4 && 2*l <= len(out); l++ {
			if slices.Equal(out[len(out)-2*l:len(out)-l], out[len(out)-l:]) {
				out = out[:len(out)-l]
				break
			}
		}
	}
	return out
}

// negations are words that turn a sentence round ("not needed"); dropping one changes the meaning.
// "t" is the end of "don't", "isn't" and the like. The Tamil ones are said, or written in English
// letters, on their own.
var negations = []string{"no", "not", "never", "nor", "none", "nothing", "without", "t",
	"illa", "illai", "venam", "vendam", "vendaam", "koodathu", "இல்லை", "இல்ல", "வேண்டாம்", "கூடாது"}

// sentenceWord is one word of a text, and whether it starts a sentence.
type sentenceWord struct {
	text  string
	start bool
}

// fact reports whether the word is a fact to keep: a number, a word that says "not", or a name (a
// capital that doesn't just start a sentence).
func (w sentenceWord) fact() bool {
	r, _ := utf8.DecodeRuneInString(w.text)
	return strings.ContainsFunc(w.text, unicode.IsDigit) || slices.Contains(negations, strings.ToLower(w.text)) ||
		unicode.IsUpper(r) && !w.start
}

// sentenceWords splits text into words. Letters, digits and the marks that Tamil letters are built
// with stay together; everything else (spaces, punctuation) separates words. A number is one word
// only when nothing separates its digits: "15,000" is the words "15" and "000". The first word, and
// a word after ".", "!", "?" or a new line, starts a sentence.
func sentenceWords(text string) []sentenceWord {
	var out []sentenceWord
	start, from := true, -1
	for i, r := range text + " " {
		inWord := unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
		switch {
		case inWord && from < 0:
			from = i
		case !inWord && from >= 0:
			out = append(out, sentenceWord{text: text[from:i], start: start})
			from, start = -1, false
		}
		if !inWord && strings.ContainsRune(".!?\n", r) {
			start = true
		}
	}
	return out
}
