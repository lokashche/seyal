//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
)

const examplePath = "../../examples/playbooks/job-evaluation.yaml"

func TestRegister(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, _ := testdb.NewTenant(t, owner, "Playbook Test")
	original := load(t, "", "")

	register := func(p *playbook.Playbook) (string, bool, error) {
		var id string
		var isNew bool
		err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
			var err error
			id, isNew, err = playbook.Register(ctx, tx, p)
			return err
		})
		return id, isNew, err
	}

	firstID, isNew, err := register(original)
	if err != nil || !isNew {
		t.Fatalf("first register = %v, new=%v; want a new version", err, isNew)
	}

	t.Run("same content again is fine", func(t *testing.T) {
		id, isNew, err := register(original)
		if err != nil || isNew || id != firstID {
			t.Errorf("register again = %q, new=%v, %v; want %q, not new", id, isNew, err, firstID)
		}
	})

	t.Run("changed content under the same version is refused", func(t *testing.T) {
		changed := load(t, "description: Evaluate", "description: Check")
		if _, _, err := register(changed); err == nil || !strings.Contains(err.Error(), "give it a new version") {
			t.Errorf("register changed = %v, want refusal", err)
		}
	})

	t.Run("changed content with a new version is a new entry", func(t *testing.T) {
		v2 := load(t, `version: "0.1"`, `version: "0.2"`)
		id, isNew, err := register(v2)
		if err != nil || !isNew || id == firstID {
			t.Errorf("register v0.2 = %q, new=%v, %v; want a new id", id, isNew, err)
		}
	})

	t.Run("each new version is in the ledger", func(t *testing.T) {
		err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM ledger WHERE kind = 'playbook.registered'").Scan(&n); err != nil {
				return err
			}
			if n != 2 {
				t.Errorf("found %d playbook.registered events, want 2 (v0.1 and v0.2)", n)
			}
			rep, err := ledger.Verify(ctx, tx, ledger.Checkpoint{})
			if err == nil && !rep.OK() {
				t.Errorf("ledger broken: %+v", rep.Problems)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("app cannot change a registered version", func(t *testing.T) {
		err := db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE playbook_versions SET source = 'changed'")
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("update = %v, want permission denied (42501)", err)
		}
	})

	t.Run("another business registers its own copy", func(t *testing.T) {
		other, _ := testdb.NewTenant(t, owner, "Playbook Test B")
		err := db.WithTenant(ctx, app, other, func(tx pgx.Tx) error {
			_, isNew, err := playbook.Register(ctx, tx, original)
			if err == nil && !isNew {
				t.Error("second business saw the first business's registration")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// load parses the sample playbook, replacing old with new first (if old is not empty).
func load(t *testing.T, old, new string) *playbook.Playbook {
	t.Helper()
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if old != "" {
		changed := strings.Replace(string(data), old, new, 1)
		if changed == string(data) {
			t.Fatalf("test setup: %q not found", old)
		}
		data = []byte(changed)
	}
	p, err := playbook.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
