// SPDX-License-Identifier: AGPL-3.0-or-later

package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"seyal/internal/media"
)

// Speak instead of type (D66). A voice note is turned into words before anything is kept: the
// person checks the words, and only when they send the message are the recording and what was heard
// saved, together with the message and in the same transaction. The message's ledger event then
// holds all three (D34): the recording's fingerprint, the words heard (raw, and tidied by AI if it
// was), and the final text, with who sent it.

// Spoken is a voice note whose words a speech service heard, sent with a message.
type Spoken struct {
	File  File  // the recording; it goes first among the message's files
	Heard Heard // what was heard in it, checked by the caller (see Heard)
}

// Heard is what the speech service heard in one recording, and what the AI tidy-up made of it. The
// caller must make sure it is genuine: the web app signs it when it turns the recording into words,
// and checks the signature when the message is sent (server/voice.go). Receive checks that the
// recording sent is the one that was heard.
type Heard struct {
	SHA256    string `json:"sha256"` // the recording's fingerprint
	Model     string `json:"model"`  // the speech service, "provider:model"
	Language  string `json:"language,omitempty"`
	Raw       string `json:"raw"`                  // the words exactly as the speech service gave them
	Tidied    string `json:"tidied,omitempty"`     // the words after the AI tidy-up; "" = not tidied
	TidyModel string `json:"tidy_model,omitempty"` // the tidy-up's model
}

// Given is the words the person was shown to check: the tidied ones, or else the raw ones.
func (h Heard) Given() string {
	if h.Tidied != "" {
		return h.Tidied
	}
	return h.Raw
}

// Transcript is what was heard in the voice note a message was sent from, as kept in the message's
// ledger snapshot. The final text is the message's text.
type Transcript struct {
	ID        string `json:"id"`
	MediaID   string `json:"media_id"`
	Model     string `json:"model"`
	Language  string `json:"language,omitempty"`
	Raw       string `json:"raw"`
	Tidied    string `json:"tidied,omitempty"`
	TidyModel string `json:"tidy_model,omitempty"`
	Edited    bool   `json:"edited"` // the person changed the words they were given before sending
}

// ErrVoiceChanged means the recording sent isn't the one whose words were heard.
var ErrVoiceChanged = errors.New("this recording isn't the one Seyal turned into words; please record it again")

// saveSpoken stores the recording of a spoken message and what was heard in it, and adds both to m.
func saveSpoken(ctx context.Context, tx pgx.Tx, store media.Store, personID string, sp Spoken, m *Message) error {
	if media.Fingerprint(sp.File.Data) != sp.Heard.SHA256 {
		return ErrVoiceChanged
	}
	if ct, ok := media.Clean(sp.File.ContentType); !ok || !strings.HasPrefix(ct, "audio/") {
		return errors.New("only voice recordings can be turned into words")
	}
	md, err := saveFile(ctx, tx, store, personID, sp.File)
	if err != nil {
		return err
	}
	h := sp.Heard
	t := &Transcript{MediaID: md.ID, Model: h.Model, Language: h.Language, Raw: cut(h.Raw), Tidied: cut(h.Tidied)}
	if t.Tidied != "" {
		t.TidyModel = h.TidyModel
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO transcripts (tenant_id, media_id, model, language, raw, tidied, tidy_model)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6) RETURNING id::text`,
		t.MediaID, t.Model, t.Language, t.Raw, t.Tidied, t.TidyModel).Scan(&t.ID); err != nil {
		return fmt.Errorf("channel: save transcript: %w", err)
	}
	t.Edited = edited(m.Text, given(t))
	m.Media = append(m.Media, md)
	m.Transcript = t
	return nil
}

// cut tidies words for keeping: valid UTF-8, no space around them, at most MaxText characters.
func cut(words string) string {
	words = strings.TrimSpace(strings.ToValidUTF8(words, ""))
	if utf8.RuneCountInString(words) > MaxText {
		words = string([]rune(words)[:MaxText])
	}
	return words
}

func given(t *Transcript) string {
	return Heard{Raw: t.Raw, Tidied: t.Tidied}.Given()
}

// TranscriptIDs returns the id of every transcript, for checks that compare them with the ledger.
// tx must come from db.WithTenant.
func TranscriptIDs(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, _ := tx.Query(ctx, `SELECT id::text FROM transcripts WHERE tenant_id = current_tenant() ORDER BY created_at, id`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// loadTranscript fills in the transcript a stored message was sent from.
func loadTranscript(ctx context.Context, tx pgx.Tx, m *Message, id string) error {
	t := &Transcript{ID: id}
	if err := tx.QueryRow(ctx, `
		SELECT media_id::text, model, language, raw, tidied, tidy_model
		FROM transcripts WHERE tenant_id = current_tenant() AND id::text = $1`, id).
		Scan(&t.MediaID, &t.Model, &t.Language, &t.Raw, &t.Tidied, &t.TidyModel); err != nil {
		return fmt.Errorf("channel: transcript %s: %w", id, err)
	}
	t.Edited = edited(m.Text, given(t))
	m.Transcript = t
	return nil
}

// edited reports whether the person changed the words they were given before sending. The composer
// adds those words after any text already in the box, so text typed before them doesn't count as a
// change. Line breaks are compared as LF (browsers send CRLF).
func edited(text, words string) bool {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == words {
		return false
	}
	before, ok := strings.CutSuffix(text, words)
	return !ok || words == "" || strings.TrimRightFunc(before, unicode.IsSpace) == before
}
