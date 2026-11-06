//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

// firm is a fictional business set up with the sample playbook, people with roles, a client
// and a work type.
type firm struct {
	t                        *testing.T
	ctx                      context.Context
	app                      *pgxpool.Pool
	ownerDB                  *pgxpool.Pool // the database owner, for setting up tests only
	tenant                   string
	pv                       string // playbook version id
	sales, ops, owner, maker string // person ids
}

func newFirm(t *testing.T) *firm {
	t.Helper()
	ctx := context.Background()
	ownerPool, app := testdb.Connect(t)
	tenant, sales := testdb.NewTenant(t, ownerPool, "Work Item Test")
	f := &firm{t: t, ctx: ctx, app: app, ownerDB: ownerPool, tenant: tenant, sales: sales}

	// Fiscal year from April, so numbering follows it.
	if _, err := ownerPool.Exec(ctx, "UPDATE tenants SET fiscal_year_start_month = 4 WHERE id = $1", tenant); err != nil {
		t.Fatal(err)
	}
	err := db.WithTenant(ctx, ownerPool, tenant, func(tx pgx.Tx) error {
		for _, role := range []string{"sales", "ops", "owner"} {
			if _, err := tx.Exec(ctx, "INSERT INTO roles (tenant_id, key, name) VALUES ($1, $2, $2)", tenant, role); err != nil {
				return err
			}
		}
		addPerson := func(name, role string) (id string, err error) {
			if err = tx.QueryRow(ctx, "INSERT INTO persons (tenant_id, display_name) VALUES ($1, $2) RETURNING id::text",
				tenant, name).Scan(&id); err != nil {
				return "", err
			}
			if role != "" {
				_, err = tx.Exec(ctx, "INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, $3)",
					tenant, id, role)
			}
			return id, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO person_roles (tenant_id, person_id, role_key) VALUES ($1, $2, 'sales')",
			tenant, sales); err != nil {
			return err
		}
		var err error
		if f.ops, err = addPerson("Olga Ops", "ops"); err != nil {
			return err
		}
		if f.owner, err = addPerson("Omar Owner", "owner"); err != nil {
			return err
		}
		f.maker, err = addPerson("Mia Maker", "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Work goes only to people with a login (D73). Mia has none, unless a test gives her one.
	testdb.GiveLogins(t, ownerPool, tenant, f.sales, f.ops, f.owner)

	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f.tx(func(tx pgx.Tx) error {
		var err error
		if f.pv, _, err = playbook.Register(ctx, tx, lib.Playbooks["job-evaluation"]); err != nil {
			return err
		}
		by := ledger.PersonActor(sales)
		if _, _, err = master.Save(ctx, tx, lib.Masters["client"], "acme", "Acme Traders", nil, by); err != nil {
			return err
		}
		if _, _, err = master.Save(ctx, tx, lib.Masters["work_type"], "monthly-books", "Monthly bookkeeping",
			engine.Fields{"standard_fee": 1200000.0, "documents": []any{"Bank statements"}, "maker": f.maker,
				"timeline_days": 10.0, "is_new": false}, by); err != nil {
			return err
		}
		_, _, err = master.Save(ctx, tx, lib.Masters["work_type"], "old-service", "Old service", nil, by)
		if err == nil {
			err = master.Deactivate(ctx, tx, "work_type", "old-service", by)
		}
		return err
	})
	return f
}

// login gives the people a made-up login, so work can go to them (D73).
func (f *firm) login(people ...string) {
	f.t.Helper()
	testdb.GiveLogins(f.t, f.ownerDB, f.tenant, people...)
}

func (f *firm) tx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := db.WithTenant(f.ctx, f.app, f.tenant, fn); err != nil {
		f.t.Fatal(err)
	}
}

// try runs fn in a transaction and returns its error instead of failing the test.
func (f *firm) try(fn func(tx pgx.Tx) error) error {
	return db.WithTenant(f.ctx, f.app, f.tenant, fn)
}

func (f *firm) start(fields engine.Fields) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Start(f.ctx, tx, workitem.NewItem{
			PlaybookVersionID: f.pv, Via: "chat", Fields: fields, By: ledger.PersonActor(f.sales)})
		return err
	})
	return it
}

func (f *firm) set(id string, fields engine.Fields) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.SetFields(f.ctx, tx, id, fields, ledger.PersonActor(f.sales))
		return err
	})
	return it
}

func (f *firm) act(id, action, reason string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Act(f.ctx, tx, id, action, reason, ledger.PersonActor(f.sales))
		return err
	})
	return it
}

func (f *firm) decide(id, person, slot, decision, reason string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Decide(f.ctx, tx, id, person, slot, decision, reason)
		return err
	})
	return it
}

func (f *firm) waitingFor(id string) []string {
	f.t.Helper()
	var st engine.ApprovalStatus
	f.tx(func(tx pgx.Tx) error {
		var err error
		st, err = workitem.Approvals(f.ctx, tx, id)
		return err
	})
	return st.Waiting
}

func (f *firm) events(id, kind string) []string {
	f.t.Helper()
	var bodies []string
	f.tx(func(tx pgx.Tx) error {
		rows, _ := tx.Query(f.ctx, "SELECT body FROM ledger WHERE subject_id::text = $1 AND kind = $2 ORDER BY seq", id, kind)
		var err error
		bodies, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return bodies
}

func (f *firm) checkLedger() {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		rep, err := ledger.Verify(f.ctx, tx, ledger.Checkpoint{})
		if err == nil && !rep.OK() {
			f.t.Errorf("ledger problems: %+v", rep.Problems)
		}
		return err
	})
}

// ready is everything the sample playbook requires before submitting, at the standard fee.
func ready() engine.Fields {
	return engine.Fields{
		"client": "acme", "nature_of_work": "monthly-books", "reason": "Monthly books for the year",
		"fee_agreed": 1200000.0, "integrity_check": "pass",
	}
}

func TestStartNumbersAndPrefills(t *testing.T) {
	f := newFirm(t)
	it := f.start(engine.Fields{"client": "acme", "nature_of_work": "monthly-books"})

	year := workitem.FiscalYear(time.Now().UTC(), 4)
	if want := fmt.Sprintf("J-%d-0001", year); it.Number != want {
		t.Errorf("number = %s, want %s", it.Number, want)
	}
	if it.State != "draft" || it.Version != 1 {
		t.Errorf("state/version = %s/%d, want draft/1", it.State, it.Version)
	}
	if it.Fields["standard_fee"] != 1200000.0 || it.Fields["maker"] != f.maker || it.Fields["timeline_days"] != 10.0 {
		t.Errorf("pre-filled fields = %v, want standard fee, maker and timeline from the work type", it.Fields)
	}
	if it.Title != "Acme Traders — Monthly bookkeeping" {
		t.Errorf("title = %q", it.Title)
	}
	if len(f.events(it.ID, "item.started")) != 1 {
		t.Error("no item.started event")
	}
	if second := f.start(nil); second.Number != fmt.Sprintf("J-%d-0002", year) {
		t.Errorf("second number = %s, want ...-0002", second.Number)
	}
	f.checkLedger()
}

func TestHappyPath(t *testing.T) {
	f := newFirm(t)
	f.login(f.maker)
	it := f.start(ready())
	it = f.act(it.ID, "submit", "")
	if it.State != "pending_approval" {
		t.Fatalf("after submit: %s, want pending_approval", it.State)
	}
	if got := f.waitingFor(it.ID); !slices.Equal(got, []string{"sales", "ops"}) {
		t.Errorf("waiting for %v, want sales and ops", got)
	}

	it = f.decide(it.ID, f.sales, "sales", engine.Approve, "")
	if it.State != "pending_approval" {
		t.Errorf("after sales only: %s, want still pending_approval", it.State)
	}
	it = f.decide(it.ID, f.ops, "ops", engine.Approve, "")
	if it.State != "approved" {
		t.Fatalf("after sales and ops: %s, want approved", it.State)
	}
	if it.AssigneeID != f.maker {
		t.Errorf("assignee = %q, want the maker (on approved: assign to maker)", it.AssigneeID)
	}
	posted := f.events(it.ID, "item.posted")
	if len(posted) != 1 || !strings.Contains(posted[0], "Approved. Over to Mia Maker.") {
		t.Errorf("posted = %v, want the approval message naming the maker", posted)
	}
	f.checkLedger()
}

func TestEditAfterApprovalResetsIt(t *testing.T) {
	f := newFirm(t)
	it := f.start(ready())
	it = f.act(it.ID, "submit", "")
	f.decide(it.ID, f.sales, "sales", engine.Approve, "")
	it = f.decide(it.ID, f.ops, "ops", engine.Approve, "")
	approvedVersion := it.Version

	it = f.set(it.ID, engine.Fields{"advance": 300000.0})
	if it.State != "pending_approval" || it.Version != approvedVersion+1 {
		t.Errorf("after edit: %s v%d, want pending_approval v%d", it.State, it.Version, approvedVersion+1)
	}
	if got := f.waitingFor(it.ID); !slices.Equal(got, []string{"sales", "ops"}) {
		t.Errorf("waiting for %v, want both approvals again", got)
	}

	if again := f.set(it.ID, engine.Fields{"advance": 300000.0}); again.Version != it.Version {
		t.Errorf("setting the same value bumped the version to %d", again.Version)
	}
	f.checkLedger()
}

func TestOwnerNeededWhenFeeIsLow(t *testing.T) {
	f := newFirm(t)
	fields := ready()
	fields["fee_agreed"] = 900000.0
	it := f.act(f.start(fields).ID, "submit", "")

	f.decide(it.ID, f.sales, "sales", engine.Approve, "")
	it = f.decide(it.ID, f.ops, "ops", engine.Approve, "")
	if it.State != "pending_approval" || !slices.Equal(f.waitingFor(it.ID), []string{"owner"}) {
		t.Fatalf("after sales and ops: %s, waiting %v; want waiting for owner", it.State, f.waitingFor(it.ID))
	}
	if it = f.decide(it.ID, f.owner, "owner", engine.Approve, ""); it.State != "approved" {
		t.Errorf("after owner: %s, want approved", it.State)
	}
	f.checkLedger()
}

func TestRejectThenSubmitAgain(t *testing.T) {
	f := newFirm(t)
	it := f.act(f.start(ready()).ID, "submit", "")
	it = f.decide(it.ID, f.ops, "ops", engine.Reject, "Scope is unclear")
	if it.State != "draft" {
		t.Fatalf("after rejection: %s, want draft", it.State)
	}

	// Submitting again starts a new approval round, even without changes.
	it = f.act(it.ID, "submit", "")
	if it.State != "pending_approval" || !slices.Equal(f.waitingFor(it.ID), []string{"sales", "ops"}) {
		t.Errorf("after resubmit: %s, waiting %v; want a fresh round", it.State, f.waitingFor(it.ID))
	}
	f.checkLedger()
}

func TestRefusals(t *testing.T) {
	f := newFirm(t)
	draft := f.start(ready())
	setErr := func(fields engine.Fields) error {
		return f.try(func(tx pgx.Tx) error {
			_, err := workitem.SetFields(f.ctx, tx, draft.ID, fields, ledger.PersonActor(f.sales))
			return err
		})
	}

	tests := []struct {
		name   string
		fields engine.Fields
	}{
		{"unknown client", engine.Fields{"client": "no-such-client"}},
		{"retired work type", engine.Fields{"nature_of_work": "old-service"}},
		{"person from nowhere", engine.Fields{"maker": "00000000-0000-0000-0000-000000000000"}},
		{"readonly standard fee", engine.Fields{"standard_fee": 1.0}},
		{"wrong type", engine.Fields{"fee_agreed": "a lot"}},
	}
	for _, tt := range tests {
		if err := setErr(tt.fields); err == nil {
			t.Errorf("%s: SetFields accepted %v", tt.name, tt.fields)
		}
	}

	incomplete := f.start(engine.Fields{"client": "acme"})
	err := f.try(func(tx pgx.Tx) error {
		_, err := workitem.Act(f.ctx, tx, incomplete.ID, "submit", "", ledger.PersonActor(f.sales))
		return err
	})
	var missing *engine.MissingError
	if !errors.As(err, &missing) {
		t.Errorf("submitting an incomplete item = %v, want a list of missing fields", err)
	}

	cancelled := f.act(draft.ID, "cancel", "Client withdrew")
	if cancelled.State != "cancelled" {
		t.Fatalf("after cancel: %s", cancelled.State)
	}
	if err := setErr(engine.Fields{"reason": "changed"}); !errors.Is(err, engine.ErrClosed) {
		t.Errorf("editing a cancelled item = %v, want ErrClosed", err)
	}
	f.checkLedger()
}

func TestLinks(t *testing.T) {
	f := newFirm(t)
	parent := f.start(ready())
	var child workitem.Item
	f.tx(func(tx pgx.Tx) error {
		var err error
		child, err = workitem.Start(f.ctx, tx, workitem.NewItem{
			PlaybookVersionID: f.pv, Via: "event", Parent: parent.ID, StartedBy: parent.ID,
			By: ledger.SystemActor})
		return err
	})
	var kinds []string
	f.tx(func(tx pgx.Tx) error {
		rows, _ := tx.Query(f.ctx, `SELECT kind FROM work_item_links
			WHERE (from_item_id::text = $1 AND to_item_id::text = $2) OR (from_item_id::text = $2 AND to_item_id::text = $1)
			ORDER BY kind`, parent.ID, child.ID)
		var err error
		kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if !slices.Equal(kinds, []string{"parent", "started_by"}) {
		t.Errorf("links = %v, want parent and started_by", kinds)
	}
	err := f.try(func(tx pgx.Tx) error {
		return workitem.Link(f.ctx, tx, parent.ID, parent.ID, "parent", ledger.SystemActor)
	})
	if err == nil {
		t.Error("an item was linked to itself")
	}
	f.checkLedger()
}

func TestOtherBusinessCannotSeeItems(t *testing.T) {
	f := newFirm(t)
	it := f.start(ready())
	g := newFirm(t)
	err := g.try(func(tx pgx.Tx) error {
		_, err := workitem.Get(g.ctx, tx, it.ID)
		return err
	})
	if !errors.Is(err, workitem.ErrNotFound) {
		t.Errorf("other business Get = %v, want ErrNotFound", err)
	}
}
