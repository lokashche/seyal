//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ledger_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/testdb"
)

const (
	codeNoPrivilege = "42501" // missing grant
	codeRaised      = "P0001" // raised by the append-only trigger
)

func TestAppendAndVerify(t *testing.T) {
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Ledger Test")

	events := appendN(t, app, tenant, person, 3)
	for i, r := range events {
		if r.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d", i+1, r.Seq)
		}
	}
	if !bytes.Equal(events[1].PrevHash, events[0].Hash) {
		t.Error("event 2 does not link to event 1")
	}
	rep := verify(t, app, tenant, ledger.Checkpoint{})
	if !rep.OK() || rep.Checked != 3 || rep.LastSeq != 3 {
		t.Errorf("verify = %+v, want OK with 3 events", rep)
	}
}

func TestEachBusinessHasItsOwnChain(t *testing.T) {
	owner, app := testdb.Connect(t)
	a, personA := testdb.NewTenant(t, owner, "Ledger Test A")
	b, personB := testdb.NewTenant(t, owner, "Ledger Test B")
	appendN(t, app, a, personA, 2)
	if got := appendN(t, app, b, personB, 1)[0].Seq; got != 1 {
		t.Errorf("first event of business B has seq %d, want 1", got)
	}
}

func TestEventsCannotBeChanged(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Ledger Test")
	appendN(t, app, tenant, person, 1)

	for _, sql := range []string{"UPDATE ledger SET kind = 'changed'", "DELETE FROM ledger", "TRUNCATE ledger"} {
		wantCode(t, db.WithTenant(ctx, app, tenant, execSQL(sql)), codeNoPrivilege, "app: "+sql)
		wantCode(t, db.WithTenant(ctx, owner, tenant, execSQL(sql)), codeRaised, "owner: "+sql)
	}
}

func TestVerifyFindsTampering(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	const rewrite = `replace(body, '"n":2', '"n":20')`

	tests := []struct {
		name    string
		tamper  string // run as owner with the append-only trigger switched off
		wantSeq int64
		wantMsg string
	}{
		{"changed content", "UPDATE ledger SET body = " + rewrite + " WHERE seq = 2", 2, "does not match its hash"},
		{"changed column", "UPDATE ledger SET kind = 'test.forged' WHERE seq = 2", 2, "kind column"},
		{"removed event", "DELETE FROM ledger WHERE seq = 2", 3, "events are missing"},
		{"rewritten event with a fresh hash",
			"UPDATE ledger SET body = " + rewrite + ", hash = sha256(prev_hash || convert_to(" + rewrite + ", 'UTF8')) WHERE seq = 2",
			3, "does not link"},
		{"removed last event", "DELETE FROM ledger WHERE seq = 3", 3, "chain head"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant, person := testdb.NewTenant(t, owner, "Ledger Test")
			appendN(t, app, tenant, person, 3)
			err := db.WithTenant(ctx, owner, tenant, func(tx pgx.Tx) error {
				for _, sql := range []string{
					"ALTER TABLE ledger DISABLE TRIGGER ledger_no_update_delete",
					tt.tamper,
					"ALTER TABLE ledger ENABLE TRIGGER ledger_no_update_delete",
				} {
					if _, err := tx.Exec(ctx, sql); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("tamper: %v", err)
			}

			rep := verify(t, app, tenant, ledger.Checkpoint{})
			for _, p := range rep.Problems {
				if p.Seq == tt.wantSeq && strings.Contains(p.Reason, tt.wantMsg) {
					return
				}
			}
			t.Errorf("verify problems = %+v, want event %d: %q", rep.Problems, tt.wantSeq, tt.wantMsg)
		})
	}
}

func TestConcurrentAppendsKeepOneChain(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Ledger Test")

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
				_, err := ledger.Append(ctx, tx, testEvent(person, 0))
				return err
			})
			if err != nil {
				t.Errorf("append: %v", err)
			}
		})
	}
	wg.Wait()

	rep := verify(t, app, tenant, ledger.Checkpoint{})
	if !rep.OK() || rep.LastSeq != n {
		t.Errorf("verify = %+v, want OK with %d events", rep, n)
	}
}

func TestVerifyFromCheckpoint(t *testing.T) {
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Ledger Test")
	appendN(t, app, tenant, person, 3)
	first := verify(t, app, tenant, ledger.Checkpoint{})

	appendN(t, app, tenant, person, 2)
	rep := verify(t, app, tenant, ledger.Checkpoint{Seq: first.LastSeq, Hash: first.LastHash})
	if !rep.OK() || rep.Checked != 2 || rep.LastSeq != 5 {
		t.Errorf("verify from checkpoint = %+v, want OK, 2 new events checked, last 5", rep)
	}

	forged := ledger.Checkpoint{Seq: first.LastSeq, Hash: bytes.Repeat([]byte{9}, 32)}
	if verify(t, app, tenant, forged).OK() {
		t.Error("verify accepted a checkpoint whose event has a different hash")
	}
}

func TestAgentEventRecordsOnBehalfOf(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Ledger Test")

	err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		_, err := ledger.Append(ctx, tx, ledger.Event{
			Kind: "test.event", SubjectType: "test", ActorKind: ledger.Agent, OnBehalfOf: person,
		})
		if err != nil {
			return err
		}
		var kind, behalf string
		err = tx.QueryRow(ctx,
			"SELECT actor_kind, on_behalf_of::text FROM ledger WHERE seq = 1").Scan(&kind, &behalf)
		if kind != "agent" || behalf != person {
			t.Errorf("stored actor_kind=%q on_behalf_of=%q, want agent and %s", kind, behalf, person)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testEvent(person string, n int) ledger.Event {
	return ledger.Event{
		Kind: "test.event", SubjectType: "test", ActorKind: ledger.Human, ActorPersonID: person,
		Payload: map[string]int{"n": n},
	}
}

// appendN adds n events to the tenant's chain, with payloads {"n":1} ... {"n":n}.
func appendN(t *testing.T, pool *pgxpool.Pool, tenant, person string, n int) []ledger.Recorded {
	t.Helper()
	var out []ledger.Recorded
	err := db.WithTenant(context.Background(), pool, tenant, func(tx pgx.Tx) error {
		for i := 1; i <= n; i++ {
			r, err := ledger.Append(context.Background(), tx, testEvent(person, i))
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return out
}

func verify(t *testing.T, pool *pgxpool.Pool, tenant string, from ledger.Checkpoint) ledger.Report {
	t.Helper()
	var rep ledger.Report
	err := db.WithTenant(context.Background(), pool, tenant, func(tx pgx.Tx) error {
		var err error
		rep, err = ledger.Verify(context.Background(), tx, from)
		return err
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return rep
}

func execSQL(sql string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql)
		return err
	}
}

func wantCode(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: got error %v, want Postgres error code %s", what, err, code)
	}
}
