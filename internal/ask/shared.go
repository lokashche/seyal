// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"seyal/internal/channel"
	"seyal/internal/engine"
)

// Shared words never act (D93; Phase 4 design 2.2.2). A message shared into Seyal from another app
// (channel.ViaShare) often holds someone else's words or voice: an outsider's "approved", "submit" or
// "cancel because …" forwarded from a chat app must never do it. So for such a message:
//   - there is no Yes or No to a suggestion, even typed (confirm is never asked);
//   - there is no typed move ("submit", "cancel because …"), approval or refusal, hand-over ("pass to
//     Dev …", "take it") or "move Ben's work to Dev": Seyal answers with sharedGuard instead;
//   - an answer to Seyal's open question becomes a suggestion with Yes and No, which only the person
//     who shared it can confirm, with a tap or their own typed "yes";
//   - the AI never reads it (understand, routing and questions about the work skip it, and later
//     messages don't send it to the model as one of the recent messages).
// Anything else is kept as a note. The person's own words, sent as a separate message, work as usual.

// sharedGuard is Seyal's reply when shared words read as a move, a decision, a Yes or No or a hand-over.
const sharedGuard = "I don't act on shared words. Use the buttons."

// sharedWords reads a shared message in an item's thread (see above). text is its trimmed words.
func (c *conversation) sharedWords(text string) error {
	if text == "" {
		return nil // only files: a note
	}
	if c.actsLike(text) {
		return c.say(sharedGuard)
	}
	f, of, err := c.pending()
	if err != nil || f == nil || of != nil {
		return err // no open question (or a yes/no offer, which shared words never answer): a note
	}
	which, err := openChoice(c.ctx, c.tx, c.reply.Item, c.person, c.reply.Message.ID)
	if err != nil || which != nil {
		return err // a "which one do you mean?" is open: shared words never pick one
	}
	v, err := Understand(*f, text, c.today, choices{c.ctx, c.tx})
	if err != nil {
		return nil // not an answer Seyal can read: a note, with no "Sorry"
	}
	return c.suggest(propose{Do: doFill, Fields: engine.Fields{f.Key: v}, Via: channel.ViaShare})
}

// actsLike reports whether text, typed by the person, would do something at once: a Yes or No to a
// suggestion, one of the process's moves, an approval or refusal, a hand-over, or moving someone's work.
func (c *conversation) actsLike(text string) bool {
	if _, ok := confirmWord(text); ok {
		return true
	}
	if c.isCommand(text) {
		return true
	}
	moves := make([]string, 0, len(c.p.Transitions))
	for _, t := range c.p.Transitions {
		moves = append(moves, t.Action)
	}
	if _, ok := readHandWords(text, moves); ok {
		return true
	}
	_, _, ok := readMoveWords(text)
	return ok
}
