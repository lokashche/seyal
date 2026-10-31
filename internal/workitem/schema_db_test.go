//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/db"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

const (
	codeNoPrivilege = "42501"
	codeDuplicate   = "23505"
)

var errRollback = errors.New("roll back on purpose")

func take(t *testing.T, app *pgxpool.Pool, tenant, key string) int64 {
	t.Helper()
	var n int64
	err := db.WithTenant(context.Background(), app, tenant, func(tx pgx.Tx) error {
		var err error
		n, err = workitem.TakeNumber(context.Background(), tx, key)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCounterIsGapFree(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, _ := testdb.NewTenant(t, owner, "Counter Test")

	for want := int64(1); want <= 3; want++ {
		if got := take(t, app, tenant, "test:2026"); got != want {
			t.Fatalf("number %d, want %d", got, want)
		}
	}

	// A transaction that takes a number and then fails must give it back.
	err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		if _, err := workitem.TakeNumber(ctx, tx, "test:2026"); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if got := take(t, app, tenant, "test:2026"); got != 4 {
		t.Errorf("after a failed transaction got %d, want 4 (no gap)", got)
	}

	if got := take(t, app, tenant, "test:2027"); got != 1 {
		t.Errorf("a different counter started at %d, want 1", got)
	}
}

func TestCounterUnderLoad(t *testing.T) {
	owner, app := testdb.Connect(t)
	tenant, _ := testdb.NewTenant(t, owner, "Counter Test")

	const n = 20
	var mu sync.Mutex
	var got []int64
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			num := take(t, app, tenant, "load")
			mu.Lock()
			got = append(got, num)
			mu.Unlock()
		})
	}
	wg.Wait()

	slices.Sort(got)
	for i, num := range got {
		if num != int64(i+1) {
			t.Fatalf("numbers taken at the same time = %v, want 1..%d with no repeats or gaps", got, n)
		}
	}
}

func TestSchemaRules(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "Schema Test")
	_, other := testdb.NewTenant(t, owner, "Schema Test B")
	second := newPerson(t, owner, tenant)

	p, err := playbook.LoadFile("../../examples/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var itemID string
	err = db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		pv, _, err := playbook.Register(ctx, tx, p)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO work_items (tenant_id, number, playbook_version_id, state, fields_sha256, started_via)
			VALUES (current_tenant(), 'T-1', $1, 'pending_approval', 'h', 'chat') RETURNING id::text`, pv).Scan(&itemID)
	})
	if err != nil {
		t.Fatal(err)
	}

	decide := func(slot, personID, decision, reason string) error {
		return db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO approvals (tenant_id, work_item_id, item_version, fields_sha256, slot, person_id, decision, reason)
				VALUES (current_tenant(), $1, 1, 'h', $2, $3, $4, nullif($5, ''))`,
				itemID, slot, personID, decision, reason)
			return err
		})
	}

	if err := decide("sales", person, "approve", ""); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	t.Run("same person cannot fill a second slot", func(t *testing.T) {
		wantCode(t, decide("ops", person, "approve", ""), codeDuplicate)
	})
	t.Run("a slot is decided once per version", func(t *testing.T) {
		wantCode(t, decide("sales", second, "approve", ""), codeDuplicate)
	})
	t.Run("a rejection needs a reason", func(t *testing.T) {
		var pgErr *pgconn.PgError
		if err := decide("ops", second, "reject", " "); !errors.As(err, &pgErr) {
			t.Errorf("reject without reason = %v, want a database error", err)
		}
	})
	t.Run("a person from another business cannot decide", func(t *testing.T) {
		if err := decide("ops", other, "approve", ""); err == nil {
			t.Error("a person of another business approved")
		}
	})
	t.Run("app cannot delete", func(t *testing.T) {
		for _, sql := range []string{"DELETE FROM work_items", "DELETE FROM approvals", "DELETE FROM master_items"} {
			err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql)
				return err
			})
			wantCode(t, err, codeNoPrivilege)
		}
	})
	t.Run("app cannot change a decision", func(t *testing.T) {
		err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE approvals SET decision = 'reject'")
			return err
		})
		wantCode(t, err, codeNoPrivilege)
	})
}

// newPerson adds another person to the tenant and returns their id.
func newPerson(t *testing.T, owner *pgxpool.Pool, tenant string) string {
	t.Helper()
	var id string
	err := db.WithTenant(context.Background(), owner, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			"INSERT INTO persons (tenant_id, display_name) VALUES ($1, 'Second Person') RETURNING id::text",
			tenant).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("got error %v, want Postgres error code %s", err, code)
	}
}
