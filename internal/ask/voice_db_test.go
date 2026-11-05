//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
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

// TestVoiceGuard: words from a voice note never run a command (no move, no cancel, no approve),
// whether they read as one exactly or the AI reads them as one. They may still answer Seyal's
// question. Typed, the same command runs.
func TestVoiceGuard(t *testing.T) {
	o := newOffice(t)
	var job string
	o.in(func(tx pgx.Tx) error {
		r, err := ask.Begin(o.ctx, tx, o.env(), o.pv, channel.Incoming{Channel: "web", PersonID: o.sam, Text: "New monthly books for Acme"})
		job = r.Item.ID
		return err
	})

	r := o.speak(job, o.sam, "cancel because test")
	o.says(r, "To do that, type it or tap the button.")
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
	o.says(r, "To do that, type it or tap the button.")
	if r.Item.State != "draft" {
		t.Fatalf("a spoken command read by AI moved the work to %s", r.Item.State)
	}

	// Typed, the command runs.
	if r = o.write(job, o.sam, "cancel because test"); r.Item.State != "cancelled" {
		t.Errorf("a typed cancel left the work in %s", r.Item.State)
	}
	o.checkLedger()
}
