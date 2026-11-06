// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import "context"

// A suggestion is Seyal asking "Pass J-2026-0007 to Dev?" with Yes and No, when AI or a voice note read
// a move or a hand-over (D68, M8; D74). When the person taps Yes, the change runs as them, with every
// check, and each event they cause names the suggestion ("suggestion": its ledger number). That link
// tells the ledger which suggestion was used, so a second Yes on it does nothing.

// suggestionKey is the key the suggestion's number is kept under in a context. It is a type of its
// own, so no other package can read or overwrite it by mistake.
type suggestionKey struct{}

// Confirming returns ctx marked with the ledger number of the suggestion a person confirmed with Yes.
// Every event a person (not Seyal on its own) causes while working with this ctx carries
// "suggestion": seq in its change.
func Confirming(ctx context.Context, seq int64) context.Context {
	return context.WithValue(ctx, suggestionKey{}, seq)
}

// confirmed returns the suggestion number Confirming put in ctx, or 0.
func confirmed(ctx context.Context) int64 {
	seq, _ := ctx.Value(suggestionKey{}).(int64)
	return seq
}
