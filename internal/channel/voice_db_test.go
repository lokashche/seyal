//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package channel_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/media"
	"seyal/internal/testdb"
)

// spoken is a recording with what a speech service heard in it (and the tidy-up, if tidied isn't "").
func spoken(audio []byte, raw, tidied string) *channel.Spoken {
	h := channel.Heard{SHA256: media.Fingerprint(audio), Model: "fake:test", Language: "tamil", Raw: raw}
	if tidied != "" {
		h.Tidied, h.TidyModel = tidied, "fake:tidy"
	}
	return &channel.Spoken{File: channel.File{ContentType: "audio/webm", Data: audio}, Heard: h}
}

// TestSpokenMessageKeepsAllThree: a message sent from a voice note keeps the recording (with its
// fingerprint), the words the service heard, the tidied words and the final text, in the tables and
// in the message's ledger event, all in one transaction; verify checks them.
func TestSpokenMessageKeepsAllThree(t *testing.T) {
	e := newEnv(t)
	audio := []byte("pretend opus: Sundaram Textiles monthly bookkeeping")
	raw := "umm Sundaram textiles-க்கு monthly book keeping start பண்ணுங்க"
	tidied := "Sundaram textiles-க்கு monthly book keeping start பண்ணுங்க."
	final := "Sundaram Textiles-க்கு monthly bookkeeping start பண்ணுங்க"
	m, _, err := e.receive(channel.Incoming{Channel: "web", PersonID: e.person, Text: final, Spoken: spoken(audio, raw, tidied)})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Media) != 1 || m.Media[0].SHA256 != media.Fingerprint(audio) || m.Media[0].ContentType != "audio/webm" {
		t.Fatalf("media = %+v", m.Media)
	}
	want := channel.Transcript{ID: m.Transcript.ID, MediaID: m.Media[0].ID, Model: "fake:test", Language: "tamil", Raw: raw,
		Tidied: tidied, TidyModel: "fake:tidy", Edited: true}
	if m.Transcript.ID == "" || *m.Transcript != want {
		t.Fatalf("transcript = %+v, want %+v", m.Transcript, want)
	}

	// The ledger: one event, by the person who sent it, with all three.
	type event struct{ Kind, ActorKind, Actor, Body string }
	var events []event
	if err := e.tx(func(tx pgx.Tx) error {
		rows, _ := tx.Query(e.ctx, `
			SELECT kind, actor_kind, coalesce(actor_person_id::text, ''), body
			FROM ledger WHERE tenant_id = current_tenant() AND kind <> 'person.added' ORDER BY seq`)
		var err error
		events, err = pgx.CollectRows(rows, pgx.RowToStructByPos[event])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "message.received" || events[0].ActorKind != "human" || events[0].Actor != e.person {
		t.Fatalf("events = %+v", events)
	}
	var sent struct {
		Payload struct {
			Message channel.Message `json:"message"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(events[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if got := sent.Payload.Message; got.Text != final || got.Media[0].SHA256 != media.Fingerprint(audio) ||
		got.Transcript == nil || *got.Transcript != want {
		t.Errorf("message in the ledger = %+v", got)
	}

	// Reading it back gives the same; the tables match the ledger.
	if err := e.tx(func(tx pgx.Tx) error {
		got, err := channel.Get(e.ctx, tx, m.ID)
		if err == nil && (got.Transcript == nil || *got.Transcript != want) {
			t.Errorf("read back: %+v", got.Transcript)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if p := e.tablesMatchLedger(); len(p) != 0 {
		t.Fatalf("tables differ from the ledger: %v", p)
	}

	// Words sent as given are not "edited": the tidied ones when tidied, else the raw ones.
	for _, c := range []struct{ raw, tidied, text string }{
		{"Ganga Traders called", "", "Ganga Traders called"},
		{"umm Ganga Traders called", "Ganga Traders called.", "Ganga Traders called."},
	} {
		m2, _, err := e.receive(channel.Incoming{Channel: "web", PersonID: e.person, Text: c.text,
			Spoken: spoken([]byte("another recording "+c.raw), c.raw, c.tidied)})
		if err != nil || m2.Transcript == nil || m2.Transcript.Edited {
			t.Errorf("words as given (%q): %+v, %v", c.text, m2.Transcript, err)
		}
	}

	// Changing the stored words afterwards (bypassing Seyal) is caught, and so is a transcript that
	// no message in the ledger was sent with.
	if err := db.WithTenant(e.ctx, e.owner, e.tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, "UPDATE transcripts SET raw = 'something else' WHERE id::text = $1", want.ID); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `INSERT INTO transcripts (tenant_id, media_id, model, raw) VALUES ($1, $2::uuid, 'fake:test', 'made up')`,
			e.tenant, want.MediaID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	problems := e.tablesMatchLedger()
	var messageProblem, extraTranscript bool
	for _, p := range problems {
		messageProblem = messageProblem || p.Table == "messages" && p.ID == m.ID && strings.Contains(p.Reason, "words")
		extraTranscript = extraTranscript || p.Table == "transcripts" && p.ID != want.ID
	}
	if !messageProblem || !extraTranscript || len(problems) != 2 {
		t.Errorf("a changed and an added transcript weren't caught: %v", problems)
	}
}

// TestSpokenRecordingMustMatch: the recording sent must be the one whose words were heard, and it
// must be audio. Nothing is kept when it isn't.
func TestSpokenRecordingMustMatch(t *testing.T) {
	e := newEnv(t)
	sp := spoken([]byte("the recording that was heard"), "my words", "")
	sp.File.Data = []byte("another recording")
	if _, _, err := e.receive(channel.Incoming{Channel: "web", PersonID: e.person, Text: "my words", Spoken: sp}); !errors.Is(err, channel.ErrVoiceChanged) {
		t.Errorf("another recording = %v, want ErrVoiceChanged", err)
	}
	photo := spoken([]byte("png"), "my words", "")
	photo.File.ContentType = "image/png"
	if _, _, err := e.receive(channel.Incoming{Channel: "web", PersonID: e.person, Text: "my words", Spoken: photo}); err == nil {
		t.Error("a photo was sent as a voice note")
	}
	var n int
	if err := e.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, "SELECT (SELECT count(*) FROM media) + (SELECT count(*) FROM transcripts) + (SELECT count(*) FROM messages)").Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("refused messages left %d rows (%v)", n, err)
	}
}

// TestTranscriptsAreNeverEdited: the app may add transcripts but not change or remove them, and a
// business sees only its own.
func TestTranscriptsAreNeverEdited(t *testing.T) {
	e := newEnv(t)
	m, _, err := e.receive(channel.Incoming{Channel: "web", PersonID: e.person, Text: "words", Spoken: spoken([]byte("words"), "words", "")})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"UPDATE transcripts SET raw = 'changed' WHERE id::text = $1",
		"DELETE FROM transcripts WHERE id::text = $1",
	} {
		err := e.tx(func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, stmt, m.Transcript.ID)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v, want permission denied", stmt, err)
		}
	}
	otherTenant, _ := testdb.NewTenant(t, e.owner, "Other Business")
	if err := db.WithTenant(e.ctx, e.app, otherTenant, func(tx pgx.Tx) error {
		ids, err := channel.TranscriptIDs(e.ctx, tx)
		if len(ids) != 0 {
			t.Errorf("another business sees transcripts: %v", ids)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
