//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
)

// TestBusinessLists: each business has its own lists. One from before that (a process in use and
// master data, but no lists) gets them from the templates; another business never sees them.
func TestBusinessLists(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	firm, _ := testdb.NewTenant(t, owner, "Lists Firm")
	turf, _ := testdb.NewTenant(t, owner, "Lists Turf")
	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	in := func(tenant string, fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, app, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}

	// The firm as it was: a process in use, no lists yet.
	in(firm, func(tx pgx.Tx) error {
		_, err := playbook.EnsureUsed(ctx, tx, lib.Playbooks["job-evaluation"], ledger.SystemActor)
		return err
	})
	in(firm, func(tx pgx.Tx) error {
		added, err := playbook.AdoptKinds(ctx, tx, lib)
		if !slices.Equal(added, []string{"client", "work_type"}) {
			t.Errorf("adopted %v, want client and work_type", added)
		}
		return err
	})
	in(firm, func(tx pgx.Tx) error {
		added, err := playbook.AdoptKinds(ctx, tx, lib)
		if len(added) != 0 {
			t.Errorf("second adoption added %v, want nothing", added)
		}
		return err
	})

	// A change the process can't live with is refused; an added detail is fine; the same text twice
	// changes nothing.
	in(firm, func(tx pgx.Tx) error {
		smaller, err := playbook.ParseMasterKind([]byte("kind: work_type\nlabel: Work type\nfields: []\n"))
		if err != nil {
			return err
		}
		if _, err := playbook.SaveKind(ctx, tx, smaller, ledger.SystemActor); err == nil ||
			!strings.Contains(err.Error(), "job-evaluation still needs it") {
			t.Errorf("removing used details = %v, want a refusal naming the process", err)
		}
		bigger, err := playbook.ParseMasterKind(append(append([]byte{}, lib.Masters["client"].Source...),
			"  - { key: city, label: City, type: text }\n"...))
		if err != nil {
			return err
		}
		for i, want := range []bool{true, false} {
			changed, err := playbook.SaveKind(ctx, tx, bigger, ledger.SystemActor)
			if err != nil || changed != want {
				t.Errorf("save %d = %v, %v; want changed=%v", i+1, changed, err, want)
			}
		}
		client, err := playbook.Kind(ctx, tx, "client")
		if err == nil && (client == nil || client.Fields[len(client.Fields)-1].Key != "city") {
			t.Errorf("client list = %+v, want the city detail", client)
		}
		return err
	})

	// The turf has none of the firm's lists; it adds a template process only with its lists.
	in(turf, func(tx pgx.Tx) error {
		kinds, err := playbook.Kinds(ctx, tx)
		if err != nil || len(kinds) != 0 {
			t.Errorf("turf lists = %v, %v; want none", kinds, err)
		}
		if err := playbook.EnsureKinds(ctx, tx, lib, lib.Playbooks["job-evaluation"], ledger.SystemActor); err != nil {
			return err
		}
		if kinds, err = playbook.Kinds(ctx, tx); err == nil && kinds["client"].Fields[len(kinds["client"].Fields)-1].Key == "city" {
			t.Error("the turf got the firm's changed client list instead of the template")
		}
		return err
	})
	in(turf, func(tx pgx.Tx) error {
		err := playbook.EnsureKinds(ctx, tx, &playbook.Library{}, lib.Playbooks["job-evaluation"], ledger.SystemActor)
		if err != nil {
			t.Errorf("lists already there: %v", err)
		}
		return nil
	})
}
