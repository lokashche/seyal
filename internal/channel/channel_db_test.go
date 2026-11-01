//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package channel_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/audit"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	owner  *pgxpool.Pool
	app    *pgxpool.Pool
	tenant string
	person string
	store  media.Disk
}

func newEnv(t *testing.T) *env {
	t.Helper()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Channel Test")
	return &env{t: t, ctx: context.Background(), owner: owner, app: app, tenant: tenant, person: person,
		store: media.Disk{Dir: t.TempDir()}}
}

func (e *env) tx(fn func(tx pgx.Tx) error) error {
	return db.WithTenant(e.ctx, e.app, e.tenant, fn)
}

func (e *env) receive(in channel.Incoming) (channel.Message, bool, error) {
	var m channel.Message
	var isNew bool
	err := e.tx(func(tx pgx.Tx) error {
		var err error
		m, isNew, err = channel.Receive(e.ctx, tx, e.store, in)
		return err
	})
	return m, isNew, err
}

func (e *env) tablesMatchLedger() []audit.Problem {
	e.t.Helper()
	var problems []audit.Problem
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		problems, err = audit.CheckTables(e.ctx, tx)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return problems
}

func TestWebChatTextAndVoice(t *testing.T) {
	e := newEnv(t)
	web := channel.Web{}

	text, isNew, err := e.receive(channel.Incoming{Channel: web.Name(), PersonID: e.person, Text: "Started the bank reconciliation"})
	if err != nil || !isNew || text.AuthorKind != "person" || text.PersonID != e.person {
		t.Fatalf("text message = %+v, new=%v, %v", text, isNew, err)
	}

	voice := []byte("OggS pretend opus audio bytes")
	note, _, err := e.receive(channel.Incoming{Channel: web.Name(), PersonID: e.person,
		Files: []channel.File{{ContentType: "audio/webm;codecs=opus", Data: voice}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(note.Media) != 1 || note.Media[0].SHA256 != media.Fingerprint(voice) || note.Media[0].Size != int64(len(voice)) {
		t.Fatalf("voice note media = %+v", note.Media)
	}
	key, _ := media.Key(e.tenant, note.Media[0].SHA256)
	r, err := e.store.Open(key)
	if err != nil {
		t.Fatalf("voice note not in the store: %v", err)
	}
	stored, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(stored, voice) {
		t.Error("stored voice note differs from what was sent")
	}

	if problems := e.tablesMatchLedger(); len(problems) != 0 {
		t.Errorf("tables differ from the ledger: %v", problems)
	}
}

func TestRepeatedDeliveryIsIgnored(t *testing.T) {
	e := newEnv(t)
	in := channel.Incoming{Channel: "web", ExternalID: "client-msg-1", PersonID: e.person, Text: "Done"}
	first, _, err := e.receive(in)
	if err != nil {
		t.Fatal(err)
	}
	again, isNew, err := e.receive(in)
	if err != nil || isNew || again.ID != first.ID {
		t.Errorf("repeat = %+v, new=%v, %v; want the same message, not new", again, isNew, err)
	}
}

func TestOtherChannelSenders(t *testing.T) {
	e := newEnv(t)
	if err := e.tx(func(tx pgx.Tx) error {
		return channel.AddIdentity(e.ctx, tx, e.person, "test-chat", "+000000000001", ledger.PersonActor(e.person))
	}); err != nil {
		t.Fatal(err)
	}
	m, _, err := e.receive(channel.Incoming{Channel: "test-chat", From: "+000000000001", Text: "On my way"})
	if err != nil || m.PersonID != e.person {
		t.Errorf("known sender = %+v, %v; want recognised as the person", m, err)
	}
	if _, _, err := e.receive(channel.Incoming{Channel: "test-chat", From: "+000000000999", Text: "Hi"}); !errors.Is(err, channel.ErrUnknownSender) {
		t.Errorf("unknown sender = %v, want ErrUnknownSender", err)
	}
}

func TestRefusedMessages(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		in   channel.Incoming
	}{
		{"empty", channel.Incoming{Channel: "web", PersonID: e.person, Text: "   "}},
		{"too long", channel.Incoming{Channel: "web", PersonID: e.person, Text: strings.Repeat("a", channel.MaxText+1)}},
		{"web page as a file", channel.Incoming{Channel: "web", PersonID: e.person,
			Files: []channel.File{{ContentType: "text/html", Data: []byte("<script>")}}}},
		{"file too big", channel.Incoming{Channel: "web", PersonID: e.person,
			Files: []channel.File{{ContentType: "audio/webm", Data: make([]byte, media.MaxSize+1)}}}},
		{"person from nowhere", channel.Incoming{Channel: "web", PersonID: "00000000-0000-0000-0000-000000000000", Text: "Hi"}},
	}
	for _, tt := range tests {
		if _, _, err := e.receive(tt.in); err == nil {
			t.Errorf("%s: message accepted", tt.name)
		}
	}
}

func TestThreads(t *testing.T) {
	e := newEnv(t)
	p, err := playbook.LoadFile("../testdata/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var item workitem.Item
	if err := e.tx(func(tx pgx.Tx) error {
		pv, _, err := playbook.Register(e.ctx, tx, p)
		if err != nil {
			return err
		}
		item, err = workitem.Start(e.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: ledger.PersonActor(e.person)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	inbox, _, err := e.receive(channel.Incoming{Channel: "web", PersonID: e.person, Text: "New job for Acme"})
	if err != nil || inbox.ThreadID != "" {
		t.Fatalf("inbox message = %+v, %v", inbox, err)
	}
	fileInto := func() (channel.Message, error) {
		var m channel.Message
		err := e.tx(func(tx pgx.Tx) error {
			var err error
			m, err = channel.FileInto(e.ctx, tx, inbox.ID, item.ID, ledger.PersonActor(e.person))
			return err
		})
		return m, err
	}
	if filed, err := fileInto(); err != nil || filed.ThreadID != item.ID {
		t.Fatalf("filed = %+v, %v", filed, err)
	}
	if _, err := fileInto(); !errors.Is(err, channel.ErrAlreadyFiled) {
		t.Errorf("filing twice = %v, want ErrAlreadyFiled", err)
	}

	var reply channel.Message
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		reply, err = channel.Post(e.ctx, tx, item.ID, "What fee did we agree?")
		return err
	}); err != nil || reply.AuthorKind != "seyal" {
		t.Fatalf("Seyal's message = %+v, %v", reply, err)
	}

	t.Run("app cannot edit a message", func(t *testing.T) {
		err := e.tx(func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, "UPDATE messages SET text = 'changed'")
			return err
		})
		if err == nil {
			t.Error("the app changed a message's text")
		}
	})

	t.Run("tables match the ledger", func(t *testing.T) {
		if problems := e.tablesMatchLedger(); len(problems) != 0 {
			t.Errorf("problems: %v", problems)
		}
	})

	t.Run("a message edited outside Seyal is found", func(t *testing.T) {
		if err := db.WithTenant(e.ctx, e.owner, e.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, "UPDATE messages SET text = 'Fee is 1 rupee' WHERE id::text = $1", reply.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		problems := e.tablesMatchLedger()
		if len(problems) != 1 || problems[0].Table != "messages" || !strings.Contains(problems[0].Reason, "text differs") {
			t.Errorf("problems = %v, want the edited message", problems)
		}
	})

	t.Run("another business can't see messages", func(t *testing.T) {
		other, _ := testdb.NewTenant(t, e.owner, "Channel Test B")
		err := db.WithTenant(e.ctx, e.app, other, func(tx pgx.Tx) error {
			_, err := channel.Get(e.ctx, tx, inbox.ID)
			return err
		})
		if err == nil {
			t.Error("another business read the message")
		}
	})
}
