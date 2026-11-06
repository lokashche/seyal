//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/channel"
	"seyal/internal/media"
)

// speak sends words that came from a voice note (with the recording and what was heard, as the web
// app does after checking its token) in a work item's thread.
func (o *office) speak(item, person, words string) ask.Reply {
	o.t.Helper()
	audio := []byte("pretend recording: " + words)
	var r ask.Reply
	o.in(func(tx pgx.Tx) error {
		var err error
		r, err = ask.Converse(o.ctx, tx, o.env(), item, channel.Incoming{Channel: "web", PersonID: person, Text: words,
			Spoken: &channel.Spoken{File: channel.File{ContentType: "audio/webm", Data: audio},
				Heard: channel.Heard{SHA256: media.Fingerprint(audio), Model: "fake:test", Raw: words}}})
		return err
	})
	return r
}

// TestVoiceGuard: words from a voice note never run a command at once (no move, no cancel, no
// approve), whether they read as one exactly or the AI reads them as one. A move becomes a suggestion
// with Yes and No (D74); approving gets the voice guard (see TestSpokenApprovalGuard). They may still
// answer Seyal's question. Typed, the same command runs.
func TestVoiceGuard(t *testing.T) {
	o := newOffice(t)
	var job string
	o.in(func(tx pgx.Tx) error {
		r, err := ask.Begin(o.ctx, tx, o.env(), o.pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: "New monthly books for Acme"})
		job = r.Item.ID
		return err
	})

	r := o.speak(job, o.sam, "cancel because test")
	o.says(r, `Do "cancel" on J-2026-0001, with the reason "test"?`)
	if r.Item.State != "draft" {
		t.Fatalf("a spoken cancel moved the work to %s", r.Item.State)
	}

	// A spoken answer to Seyal's question is used.
	r = o.speak(job, o.sam, "acme")
	if r.Item.Fields["client"] != "acme" {
		t.Errorf("a spoken answer wasn't used: fields %v", r.Item.Fields)
	}

	// The AI reads a spoken message as a command: still nothing runs.
	o.withFake(map[string]string{"send it in": `{"kind":"command","fields":{},"new_items":[],"command":"submit","reason":"","reply":""}`})
	r = o.speak(job, o.sam, "please send it in now")
	o.says(r, "I couldn't submit it: still needed") // refused now, so nothing is suggested
	if r.Item.State != "draft" {
		t.Fatalf("a spoken command read by AI moved the work to %s", r.Item.State)
	}

	// Typed, the command runs.
	if r = o.write(job, o.sam, "cancel because test"); r.Item.State != "cancelled" {
		t.Errorf("a typed cancel left the work in %s", r.Item.State)
	}
	o.checkLedger()
}

// TestSpokenApprovalGuard: approving or refusing in a voice note gets the voice guard. It is not a
// suggestion, so no Yes and No, and no decision is recorded (D41; moving-work design, decision 15).
// Typed, the approval is recorded.
func TestSpokenApprovalGuard(t *testing.T) {
	o := newOffice(t)
	o.withFake(map[string]string{
		"Acme": `{"kind":"new_work","fields":{"client":"Acme","nature_of_work":"monthly","reason":"books",
			"fee_agreed":"12000","integrity_check":"pass"},"new_items":[],"command":"","reason":"","reply":""}`,
	})
	job := o.begin("Acme monthly books, 12000, pass, for books").Item.ID
	if r := o.write(job, o.sam, "submit"); r.Item.State != "pending_approval" {
		t.Fatalf("after submit: %s; Seyal said %q", r.Item.State, said(r))
	}
	for _, words := range []string{"approve", "reject because the scope is unclear"} {
		r := o.speak(job, o.sam, words)
		o.says(r, "To approve or refuse, type it or tap the button.")
		if r.Item.State != "pending_approval" {
			t.Errorf("spoken %q moved the work to %s", words, r.Item.State)
		}
		if q := o.buttons(job, o.sam); q != nil {
			t.Errorf("spoken %q: buttons %+v; want none (no suggestion)", words, q)
		}
	}
	if n := o.count("SELECT count(*) FROM ledger WHERE kind = 'item.decided'"); n != 0 {
		t.Errorf("%d decisions recorded from voice notes, want 0", n)
	}
	if r := o.write(job, o.sam, "approve"); !strings.Contains(said(r), "Waiting for: ops") {
		t.Errorf("a typed approve: Seyal said %q", said(r))
	}
	if n := o.count("SELECT count(*) FROM ledger WHERE kind = 'item.decided'"); n != 1 {
		t.Errorf("%d decisions after a typed approve, want 1", n)
	}
	o.checkLedger()
}
