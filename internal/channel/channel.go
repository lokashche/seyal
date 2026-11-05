// SPDX-License-Identifier: AGPL-3.0-or-later

// Package channel is how messages come into and go out of Seyal. Every channel (the app's own web
// chat now, others later) uses one message format, so the core never depends on a particular
// channel.
package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
	"seyal/internal/media"
)

// Adapter connects one channel to Seyal.
type Adapter interface {
	// Name is stored with every message from this channel, e.g. "web".
	Name() string
	// Deliver passes a message from Seyal on to a person. The web chat has nothing to do here,
	// because its screen shows the stored messages; other channels will call their service.
	Deliver(ctx context.Context, personID string, m Message) error
}

// Web is the app's own chat.
type Web struct{}

func (Web) Name() string                                   { return "web" }
func (Web) Deliver(context.Context, string, Message) error { return nil }

// Message is a stored message in the one format every channel shares. It is also the snapshot
// recorded in the ledger.
type Message struct {
	ID         string    `json:"id"`
	ThreadID   string    `json:"thread_id,omitempty"` // work item id; "" = not yet filed
	AuthorKind string    `json:"author_kind"`         // "person" or "seyal"
	PersonID   string    `json:"person_id,omitempty"`
	Channel    string    `json:"channel"`
	ExternalID string    `json:"external_id,omitempty"` // the channel's own id for the message
	Text       string    `json:"text"`
	Media      []Media   `json:"media,omitempty"`
	SentAt     time.Time `json:"sent_at"`
	// Transcript is set when the text came from a voice note (speak instead of type): the words the
	// speech service heard (and the AI tidy-up's, if used), next to the final text the person sent
	// (voice.go).
	Transcript *Transcript `json:"transcript,omitempty"`
}

// Media describes one stored file. The bytes are in the media store, under the fingerprint.
type Media struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// Incoming is a message as a channel adapter hands it to Seyal.
type Incoming struct {
	Channel    string
	ExternalID string // optional; a repeat with the same id is ignored
	PersonID   string // for channels where the sender is logged in (the web chat)
	From       string // the sender's id on other channels, matched via channel identities
	Text       string
	Files      []File
	// Spoken is set when the text came from a voice note (speak instead of type): the recording and
	// what was heard in it. The recording goes first among the message's files (voice.go).
	Spoken   *Spoken
	ThreadID string    // optional work item id
	SentAt   time.Time // optional; now if zero
}

// File is a file sent with a message.
type File struct {
	ContentType string
	Data        []byte
}

// MaxText is the longest message text accepted, in characters.
const MaxText = 10000

var (
	ErrUnknownSender = errors.New("unknown sender")
	ErrEmpty         = errors.New("a message needs text or a file")
	ErrAlreadyFiled  = errors.New("the message is already in a thread")
)

// Receive stores an incoming message and its files and records message.received. A message the
// channel already delivered (same ExternalID) is returned as it is, with isNew false, so a channel
// retrying is harmless. tx must come from db.WithTenant.
//
// Files are written to the store before the transaction ends. If the transaction then fails, the
// file stays in the store with nothing pointing at it; that's harmless, because files are stored by
// fingerprint and never served without a message.
func Receive(ctx context.Context, tx pgx.Tx, store media.Store, in Incoming) (m Message, isNew bool, err error) {
	if strings.TrimSpace(in.Text) == "" && len(in.Files) == 0 && in.Spoken == nil {
		return Message{}, false, ErrEmpty
	}
	if utf8.RuneCountInString(in.Text) > MaxText {
		return Message{}, false, fmt.Errorf("a message can have at most %d characters", MaxText)
	}
	if in.ExternalID != "" {
		m, err := load(ctx, tx, "channel = $1 AND external_id = $2", in.Channel, in.ExternalID)
		switch {
		case err == nil:
			return m, false, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Message{}, false, err
		}
	}
	personID, identityID, err := sender(ctx, tx, in)
	if err != nil {
		return Message{}, false, err
	}

	m = Message{
		ThreadID: in.ThreadID, AuthorKind: "person", PersonID: personID, Channel: in.Channel,
		ExternalID: in.ExternalID, Text: in.Text, SentAt: in.SentAt,
	}
	if m.SentAt.IsZero() {
		m.SentAt = time.Now()
	}
	m.SentAt = m.SentAt.UTC().Truncate(time.Microsecond) // Postgres keeps microseconds
	if in.Spoken != nil {
		if err := saveSpoken(ctx, tx, store, personID, *in.Spoken, &m); err != nil {
			return Message{}, false, err
		}
	}
	for _, f := range in.Files {
		md, err := saveFile(ctx, tx, store, personID, f)
		if err != nil {
			return Message{}, false, err
		}
		m.Media = append(m.Media, md)
	}
	if err := insert(ctx, tx, &m, identityID); err != nil {
		return Message{}, false, err
	}
	if err := record(ctx, tx, "message.received", m, ledger.PersonActor(personID)); err != nil {
		return Message{}, false, err
	}
	return m, true, nil
}

// Post stores a message from Seyal itself, in a thread (or "" for none), and records message.sent.
// tx must come from db.WithTenant.
func Post(ctx context.Context, tx pgx.Tx, threadID, text string) (Message, error) {
	if strings.TrimSpace(text) == "" {
		return Message{}, ErrEmpty
	}
	m := Message{
		ThreadID: threadID, AuthorKind: "seyal", Channel: "seyal", Text: text,
		SentAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := insert(ctx, tx, &m, ""); err != nil {
		return Message{}, err
	}
	if err := record(ctx, tx, "message.sent", m, ledger.SystemActor); err != nil {
		return Message{}, err
	}
	return m, nil
}

// FileInto puts a message that isn't in a thread yet into a work item's thread.
// tx must come from db.WithTenant.
func FileInto(ctx context.Context, tx pgx.Tx, messageID, threadID string, by ledger.Actor) (Message, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE messages SET work_item_id = $2::uuid
		WHERE tenant_id = current_tenant() AND id::text = $1 AND work_item_id IS NULL`, messageID, threadID)
	if err != nil {
		return Message{}, fmt.Errorf("channel: file message: %w", err)
	}
	m, err := Get(ctx, tx, messageID)
	if err != nil {
		return Message{}, err
	}
	if tag.RowsAffected() == 0 {
		return Message{}, ErrAlreadyFiled
	}
	return m, record(ctx, tx, "message.filed", m, by)
}

// AddIdentity records that a person uses externalID on a channel, so their messages there are
// recognised. tx must come from db.WithTenant.
func AddIdentity(ctx context.Context, tx pgx.Tx, personID, channel, externalID string, by ledger.Actor) error {
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO channel_identities (tenant_id, person_id, channel, external_id)
		VALUES (current_tenant(), $1, $2, $3) RETURNING id::text`, personID, channel, externalID).Scan(&id); err != nil {
		return fmt.Errorf("channel: add identity: %w", err)
	}
	_, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: "person.identity_added", SubjectType: "person", SubjectID: personID,
		Payload: map[string]string{"identity_id": id, "channel": channel, "external_id": externalID},
	}.By(by))
	return err
}

// Get returns one message with its files. tx must come from db.WithTenant.
func Get(ctx context.Context, tx pgx.Tx, id string) (Message, error) {
	m, err := load(ctx, tx, "id::text = $1", id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, fmt.Errorf("message %s not found", id)
	}
	return m, err
}

// List returns all messages, oldest first, for checks that compare them with the ledger.
// tx must come from db.WithTenant.
func List(ctx context.Context, tx pgx.Tx) ([]Message, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM messages WHERE tenant_id = current_tenant() ORDER BY sent_at, id`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		m, err := Get(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func sender(ctx context.Context, tx pgx.Tx, in Incoming) (personID, identityID string, err error) {
	if in.PersonID != "" {
		var ok bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM persons
				WHERE tenant_id = current_tenant() AND id::text = $1 AND status = 'active')`, in.PersonID).Scan(&ok)
		if err == nil && !ok {
			err = ErrUnknownSender
		}
		return in.PersonID, "", err
	}
	err = tx.QueryRow(ctx, `
		SELECT ci.person_id::text, ci.id::text
		FROM channel_identities ci
		JOIN persons p ON p.tenant_id = ci.tenant_id AND p.id = ci.person_id
		WHERE ci.tenant_id = current_tenant() AND ci.channel = $1 AND ci.external_id = $2
			AND p.status = 'active'`, in.Channel, in.From).Scan(&personID, &identityID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = fmt.Errorf("%s %q: %w", in.Channel, in.From, ErrUnknownSender)
	}
	return personID, identityID, err
}

func saveFile(ctx context.Context, tx pgx.Tx, store media.Store, personID string, f File) (Media, error) {
	contentType, ok := media.Clean(f.ContentType)
	switch {
	case len(f.Data) == 0:
		return Media{}, errors.New("an attached file is empty")
	case len(f.Data) > media.MaxSize:
		return Media{}, fmt.Errorf("files can be at most %d MB", media.MaxSize>>20)
	case !ok:
		return Media{}, fmt.Errorf("files of type %q are not accepted (voice notes, photos and PDFs are)", f.ContentType)
	}
	f.ContentType = contentType // only the plain type is kept
	var tenant string
	if err := tx.QueryRow(ctx, "SELECT current_tenant()::text").Scan(&tenant); err != nil {
		return Media{}, err
	}
	md := Media{ContentType: f.ContentType, Size: int64(len(f.Data)), SHA256: media.Fingerprint(f.Data)}
	key, err := media.Key(tenant, md.SHA256)
	if err != nil {
		return Media{}, err
	}
	if err := store.Put(key, f.Data); err != nil {
		return Media{}, fmt.Errorf("channel: store file: %w", err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO media (tenant_id, uploaded_by, content_type, size, sha256, storage_key)
		VALUES (current_tenant(), $1, $2, $3, $4, $5) RETURNING id::text`,
		personID, md.ContentType, md.Size, md.SHA256, key).Scan(&md.ID)
	if err != nil {
		return Media{}, fmt.Errorf("channel: record file: %w", err)
	}
	return md, nil
}

func insert(ctx context.Context, tx pgx.Tx, m *Message, identityID string) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO messages (tenant_id, work_item_id, author_kind, author_person_id, channel,
			channel_identity_id, external_id, text, sent_at, transcript_id)
		VALUES (current_tenant(), nullif($1::text, '')::uuid, $2, nullif($3::text, '')::uuid, $4,
			nullif($5::text, '')::uuid, nullif($6::text, ''), $7, $8, nullif($9::text, '')::uuid)
		RETURNING id::text`,
		m.ThreadID, m.AuthorKind, m.PersonID, m.Channel, identityID, m.ExternalID, m.Text, m.SentAt, transcriptID(m)).Scan(&m.ID)
	if err != nil {
		return fmt.Errorf("channel: save message: %w", err)
	}
	for i, md := range m.Media {
		if _, err := tx.Exec(ctx, `
			INSERT INTO message_media (tenant_id, message_id, media_id, position)
			VALUES (current_tenant(), $1, $2, $3)`, m.ID, md.ID, i); err != nil {
			return fmt.Errorf("channel: attach file: %w", err)
		}
	}
	return nil
}

func transcriptID(m *Message) string {
	if m.Transcript == nil {
		return ""
	}
	return m.Transcript.ID
}

const selectMessage = `
	SELECT id::text, coalesce(work_item_id::text, ''), author_kind, coalesce(author_person_id::text, ''),
		channel, coalesce(external_id, ''), text, sent_at, coalesce(transcript_id::text, '')
	FROM messages WHERE tenant_id = current_tenant() AND `

func load(ctx context.Context, tx pgx.Tx, where string, args ...any) (Message, error) {
	var m Message
	var transcript string
	err := tx.QueryRow(ctx, selectMessage+where, args...).
		Scan(&m.ID, &m.ThreadID, &m.AuthorKind, &m.PersonID, &m.Channel, &m.ExternalID, &m.Text, &m.SentAt, &transcript)
	if err != nil {
		return Message{}, err
	}
	if transcript != "" {
		if err := loadTranscript(ctx, tx, &m, transcript); err != nil {
			return Message{}, err
		}
	}
	rows, _ := tx.Query(ctx, `
		SELECT md.id::text, md.content_type, md.size, md.sha256
		FROM message_media mm JOIN media md ON md.tenant_id = mm.tenant_id AND md.id = mm.media_id
		WHERE mm.tenant_id = current_tenant() AND mm.message_id::text = $1
		ORDER BY mm.position`, m.ID)
	files, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Media])
	if err != nil {
		return Message{}, err
	}
	if len(files) > 0 {
		m.Media = files
	}
	return m, nil
}

func record(ctx context.Context, tx pgx.Tx, kind string, m Message, by ledger.Actor) error {
	_, err := ledger.Append(ctx, tx, ledger.Event{
		Kind: kind, SubjectType: "message", SubjectID: m.ID, Payload: map[string]any{"message": m},
	}.By(by))
	return err
}
