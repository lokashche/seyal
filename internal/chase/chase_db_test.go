//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/audit"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

// A made-up process with one open step, for the chaser's tests.
const testProcess = `id: chase-test
version: "1"
description: A made-up process for the chaser's tests.
numbering: "C-{seq:4}"
title: "{what}"
fields:
  - { key: what, label: What, type: text, required: true }
states: [open, done]
initial: open
transitions:
  - { action: finish, from: [open], to: done }
`

// place is one fictional business with some open items.
type place struct {
	tenant db.Tenant
	items  map[string]string // title → item id
	pv     string            // the made-up process's version id
	person string            // the fictional person who starts the items
}

func newPlace(t *testing.T, owner, app *pgxpool.Pool, titles ...string) place {
	t.Helper()
	ctx := context.Background()
	id, person := testdb.NewTenant(t, owner, "Chase Test Traders")
	testdb.GiveLogins(t, owner, id, person)
	pl := place{items: map[string]string{}, person: person}
	pl.tenant.ID = id
	err := db.WithTenant(ctx, app, id, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT slug FROM tenants WHERE id = current_tenant()").Scan(&pl.tenant.Slug); err != nil {
			return err
		}
		p, err := playbook.Parse([]byte(testProcess))
		if err == nil {
			err = p.Validate()
		}
		if err != nil {
			return err
		}
		if pl.pv, _, err = playbook.Register(ctx, tx, p); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range titles {
		pl.start(t, app, title)
	}
	return pl
}

// start starts one more open item in the business, as its person.
func (pl place) start(t *testing.T, app *pgxpool.Pool, title string) {
	t.Helper()
	ctx := context.Background()
	err := db.WithTenant(ctx, app, pl.tenant.ID, func(tx pgx.Tx) error {
		it, err := workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pl.pv, Via: "chat",
			Fields: engine.Fields{"what": title}, By: ledger.PersonActor(pl.person)})
		if err == nil {
			pl.items[title] = it.ID
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// testReminder is a test-only finder, shaped like the reminders of part 2: every open item not yet
// reminded in this step stay gets one line in its thread, by Seyal, with reminded_at and reminders set
// on the item in the same transaction. An item titled "panic" or "fail" breaks in that way. It records
// which items it saw in each business, and how often each title was tried.
type testReminder struct {
	mu    sync.Mutex
	seen  map[string][]string // business id → item ids the finder saw
	tries map[string]int      // title → times Do ran
}

func newTestReminder() *testReminder {
	return &testReminder{seen: map[string][]string{}, tries: map[string]int{}}
}

func (r *testReminder) find(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error) {
	items, err := workitem.List(ctx, tx)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, it := range items {
		r.mu.Lock()
		r.seen[b.ID] = append(r.seen[b.ID], it.ID)
		r.mu.Unlock()
		if it.StepSince == "" || it.RemindedAt != "" {
			continue
		}
		since, err := time.Parse(workitem.HeldSinceLayout, it.StepSince)
		if err != nil {
			return nil, err
		}
		id := it.ID
		tasks = append(tasks, Task{Ref: it.Number, Key: id, Due: since, Do: func(ctx context.Context, tx pgx.Tx) (string, error) {
			it, err := workitem.Get(ctx, tx, id) // read again, under the ledger lock
			if err != nil {
				return "", err
			}
			r.mu.Lock()
			r.tries[it.Title]++
			r.mu.Unlock()
			if it.RemindedAt != "" {
				return "", nil // another round got here first
			}
			switch it.Title {
			case "panic":
				var nothing map[string]int
				nothing["boom"]++ // a bug in Go code: writing to a nil map panics
			case "fail":
				return "", errors.New("this item is broken")
			}
			it.RemindedAt, it.Reminders = b.Now.UTC().Truncate(time.Microsecond).Format(workitem.HeldSinceLayout), 1
			if _, err := tx.Exec(ctx, `UPDATE work_items SET reminded_at = $2::timestamptz, reminders = $3
				WHERE tenant_id = current_tenant() AND id::text = $1`, it.ID, it.RemindedAt, it.Reminders); err != nil {
				return "", err
			}
			change := map[string]any{"text": "Test reminder", "chase": "test"}
			if b.AsIf != "" {
				change["as_if"] = b.AsIf
			}
			_, err = ledger.Append(ctx, tx, ledger.Event{Kind: "item.posted", SubjectType: "work_item", SubjectID: it.ID,
				Payload: map[string]any{"change": change, "item": it}}.By(ledger.SystemActor))
			if err != nil {
				return "", err
			}
			return "reminded holder", nil
		}})
	}
	return tasks, nil
}

func (r *testReminder) chaser(app *pgxpool.Pool, out *bytes.Buffer, places ...place) *Chaser {
	c := New(app, log.New(out, "", 0))
	c.Finders = []Finder{r.find}
	c.List = func(context.Context) ([]db.Tenant, error) {
		var list []db.Tenant
		for _, pl := range places {
			list = append(list, pl.tenant)
		}
		return list, nil
	}
	return c
}

// reminders counts the test reminders in an item's thread, and checks they are by Seyal (system).
func reminders(t *testing.T, app *pgxpool.Pool, pl place, title string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := db.WithTenant(ctx, app, pl.tenant.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE subject_id::text = $1 AND kind = 'item.posted'
			AND actor_kind = 'system' AND body::jsonb #>> '{payload,change,chase}' = 'test'`, pl.items[title]).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// clean checks that `seyal verify` finds nothing wrong in the business: the chain and the tables.
func clean(t *testing.T, app *pgxpool.Pool, pl place) {
	t.Helper()
	ctx := context.Background()
	err := db.WithTenant(ctx, app, pl.tenant.ID, func(tx pgx.Tx) error {
		rep, err := ledger.Verify(ctx, tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Errorf("%s: ledger problems: %+v", pl.tenant.Slug, rep.Problems)
		}
		problems, err := audit.CheckTables(ctx, tx)
		if len(problems) > 0 {
			t.Errorf("%s: tables differ from the ledger: %v", pl.tenant.Slug, problems)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRoundSeesEachBusinessAlone: a round over two businesses sees only each one's items (row-level
// security, as for a web request), reminds each item once, and writes the reminder and the item row
// together, so `seyal verify` stays clean. Two copies of the app chasing at the same moment still
// remind each item only once, and a round with nothing due writes nothing.
func TestRoundSeesEachBusinessAlone(t *testing.T) {
	owner, app := testdb.Connect(t)
	a := newPlace(t, owner, app, "first", "second")
	b := newPlace(t, owner, app, "third")
	r := newTestReminder()

	var out1, out2 bytes.Buffer
	one, two := r.chaser(app, &out1, a, b), r.chaser(app, &out2, a, b)
	var wg sync.WaitGroup
	var done1, done2 int
	wg.Go(func() { done1 = one.Round(context.Background()) })
	wg.Go(func() { done2 = two.Round(context.Background()) })
	wg.Wait()
	if done1+done2 != 3 {
		t.Errorf("two copies did %d + %d things, want 3 in all; logs:\n%s\n%s", done1, done2, out1.String(), out2.String())
	}

	for _, pl := range []place{a, b} {
		want := map[string]bool{}
		for _, id := range pl.items {
			want[id] = true
		}
		for _, id := range r.seen[pl.tenant.ID] {
			if !want[id] {
				t.Errorf("%s's round saw item %s of another business", pl.tenant.Slug, id)
			}
		}
		for title := range pl.items {
			if n := reminders(t, app, pl, title); n != 1 {
				t.Errorf("%s %q: %d reminders, want exactly 1", pl.tenant.Slug, title, n)
			}
		}
		clean(t, app, pl)
	}
	logs := out1.String() + out2.String()
	if strings.Count(logs, ": reminded holder") != 3 || !strings.Contains(logs, "chase "+a.tenant.Slug+": C-0001: reminded holder") {
		t.Errorf("log = %q; want one line per reminder, with the business and the item number", logs)
	}
	if strings.Contains(logs, "Test Person") {
		t.Errorf("the log names a person: %q", logs)
	}

	var again bytes.Buffer
	if n := r.chaser(app, &again, a, b).Round(context.Background()); n != 0 || again.Len() != 0 {
		t.Errorf("a round with nothing due did %d things and logged %q; want nothing", n, again.String())
	}
}

// TestOneFailingItemDoesNotStopOthers: an item whose action panics and one whose action fails are logged
// once and skipped for the rest of the day; the items after them still run. Only actions that worked
// count toward the cap, and the rest wait for the next round.
func TestOneFailingItemDoesNotStopOthers(t *testing.T) {
	owner, app := testdb.Connect(t)
	pl := newPlace(t, owner, app, "panic", "fail", "fine 1", "fine 2", "fine 3")
	r := newTestReminder()
	var out bytes.Buffer
	c := r.chaser(app, &out, pl)
	c.Limits = Limits{MaxActions: 2}

	if n := c.Round(context.Background()); n != 2 {
		t.Errorf("first round did %d things, want 2 (the cap); log: %s", n, out.String())
	}
	if reminders(t, app, pl, "fine 1") != 1 || reminders(t, app, pl, "fine 2") != 1 || reminders(t, app, pl, "fine 3") != 0 {
		t.Errorf("after the first round: the oldest two fine items should be reminded, the third should wait")
	}
	if !strings.Contains(out.String(), "C-0001: failed, skipped until tomorrow: panic") ||
		!strings.Contains(out.String(), "C-0002: failed, skipped until tomorrow: this item is broken") {
		t.Errorf("log = %q; want the panic and the failure, each with its item number", out.String())
	}
	if n := c.Round(context.Background()); n != 1 || reminders(t, app, pl, "fine 3") != 1 {
		t.Errorf("second round did %d things, want 1: the third fine item", n)
	}
	c.Round(context.Background())
	if r.tries["panic"] != 1 || r.tries["fail"] != 1 {
		t.Errorf("the broken items were tried %d and %d times, want once each (then skipped for the day)",
			r.tries["panic"], r.tries["fail"])
	}
	if strings.Count(out.String(), "failed") != 2 {
		t.Errorf("log = %q; want each failure logged once", out.String())
	}
	if reminders(t, app, pl, "panic") != 0 || reminders(t, app, pl, "fail") != 0 {
		t.Error("a failed action left a reminder behind")
	}
	clean(t, app, pl)
}

// TestBusyBusinessDoesNotStopAnother: while someone holds one business's ledger lock, its items wait a
// short time each (the lock time limit), the business stops at its own time limit, and the next business
// is still chased. Busy items aren't skipped for the day: the next round does them.
func TestBusyBusinessDoesNotStopAnother(t *testing.T) {
	owner, app := testdb.Connect(t)
	ctx := context.Background()
	busyPlace := newPlace(t, owner, app, "first", "second", "third")
	other := newPlace(t, owner, app, "elsewhere")

	hold, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if err := db.SetScope(ctx, hold, db.Scope{Tenant: busyPlace.tenant.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Lock(ctx, hold); err != nil {
		t.Fatal(err)
	}

	r := newTestReminder()
	var out bytes.Buffer
	c := r.chaser(app, &out, busyPlace, other)
	// Room enough that a busy test database (other packages run at the same time) can't use up the
	// business's time before its first item reaches the lock: two items wait 600ms each, the third
	// would pass 1.5s, so the business stops at its own limit.
	c.Limits = Limits{LockWait: 600 * time.Millisecond, Business: 1500 * time.Millisecond}
	started := time.Now()
	if n := c.Round(ctx); n != 1 || reminders(t, app, other, "elsewhere") != 1 {
		t.Errorf("round did %d things, want 1: the other business's item; log: %s", n, out.String())
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("the round took %s; the busy business should stop at its own limit", took)
	}
	if !strings.Contains(out.String(), "C-0001: busy, tried again next round") {
		t.Errorf("log = %q; want the busy item named", out.String())
	}
	if !strings.Contains(out.String(), "chase "+busyPlace.tenant.Slug+": stopped after 1.5s") {
		t.Errorf("log = %q; want the busy business stopped at its time limit", out.String())
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := c.Round(ctx); n != 3 {
		t.Errorf("after the lock was let go, the round did %d things, want 3; log: %s", n, out.String())
	}
	clean(t, app, busyPlace)
	clean(t, app, other)
}

// TestClockBehindDoesNothing: with the app's clock an hour behind the database's (and so behind the
// business's last ledger event), a round does nothing and says why in the log. The check against the
// last event works on its own too: a business whose last event is later than the app's clock is left
// alone, so a clock that goes back can't cause a second reminder.
func TestClockBehindDoesNothing(t *testing.T) {
	owner, app := testdb.Connect(t)
	pl := newPlace(t, owner, app, "first")
	r := newTestReminder()
	var out bytes.Buffer
	c := r.chaser(app, &out, pl)
	behind := time.Now().Add(-time.Hour)
	c.Now = func() time.Time { return behind }

	if n := c.Round(context.Background()); n != 0 || reminders(t, app, pl, "first") != 0 {
		t.Errorf("round with the clock behind did %d things, want none", n)
	}
	if !strings.Contains(out.String(), "away from the database's clock; this round does nothing") {
		t.Errorf("log = %q; want it to say the clock is off", out.String())
	}
	n, err := c.business(context.Background(), pl.tenant, "")
	if n != 0 || err == nil || !strings.Contains(err.Error(), "behind the last ledger event") {
		t.Errorf("business with the clock behind its last event = %d, %v; want nothing done and why", n, err)
	}
	if len(r.tries) != 0 || reminders(t, app, pl, "first") != 0 {
		t.Error("something was tried with the clock behind")
	}
	clean(t, app, pl)
}

// TestClockReadForEachBusiness: each business reads the clock again, so a round that takes a while
// doesn't refuse a later business as "clock behind" when someone just did something there. The round
// starts with the app's clock almost 2 minutes behind (still allowed); while the first business is
// looked at, time goes on and someone starts an item in the second. One reading for the whole round
// would now be more than 2 minutes behind the second business's last event.
func TestClockReadForEachBusiness(t *testing.T) {
	owner, app := testdb.Connect(t)
	first := newPlace(t, owner, app)
	second := newPlace(t, owner, app, "older")
	r := newTestReminder()
	var out bytes.Buffer
	c := r.chaser(app, &out, first, second)
	reads := 0
	c.Now = func() time.Time {
		reads++
		if reads == 1 {
			return time.Now().Add(-115 * time.Second) // the round's start: behind, within the 2 minutes
		}
		return time.Now()
	}
	c.Finders = []Finder{func(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error) {
		if b.ID == first.tenant.ID {
			time.Sleep(6 * time.Second) // a slow first business
			second.start(t, app, "just now")
		}
		return r.find(ctx, tx, b)
	}}
	if n := c.Round(context.Background()); n != 2 || strings.Contains(out.String(), "behind") {
		t.Errorf("round did %d things, want 2 (both of the second business's items); log: %s", n, out.String())
	}
	clean(t, app, second)
}

// TestFinderPanicDoesNotStopRound: a finder that panics in one business is logged as that business's
// problem, and the next business is still chased (the web server, in the same program, keeps running).
func TestFinderPanicDoesNotStopRound(t *testing.T) {
	owner, app := testdb.Connect(t)
	broken := newPlace(t, owner, app, "unread")
	fine := newPlace(t, owner, app, "fine")
	r := newTestReminder()
	var out bytes.Buffer
	c := r.chaser(app, &out, broken, fine)
	c.Finders = []Finder{func(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error) {
		if b.ID == broken.tenant.ID {
			var nothing []Task
			_ = nothing[3] // a bug while reading: index out of range panics
		}
		return r.find(ctx, tx, b)
	}}
	if n := c.Round(context.Background()); n != 1 || reminders(t, app, fine, "fine") != 1 {
		t.Errorf("round did %d things, want 1: the other business's item; log: %s", n, out.String())
	}
	if !strings.Contains(out.String(), "chase "+broken.tenant.Slug+": panic while looking for work") {
		t.Errorf("log = %q; want the panic, with the business", out.String())
	}
	if reminders(t, app, broken, "unread") != 0 {
		t.Error("the broken business's item was reminded")
	}
	clean(t, app, broken)
	clean(t, app, fine)
}

// TestAsIf: `dev chase --as-if` works out what is due from the pretend day, but stores real times and
// adds "as_if" to its events. It refuses a day before the real today, and a day before the last pretend
// day in the ledger, so time never runs backwards. (The demo-only rule is in TestAsIfRefusesBadDays.)
func TestAsIf(t *testing.T) {
	owner, app := testdb.Connect(t)
	ctx := context.Background()
	pl := newPlace(t, owner, app, "first", "second")
	r := newTestReminder()
	var out bytes.Buffer
	c := r.chaser(app, &out, pl)

	var today time.Time
	var lastDay time.Time
	c.Finders = []Finder{func(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error) {
		lastDay = b.Today
		if b.AsIf != "" && b.Today.Format(time.DateOnly) != b.AsIf {
			t.Errorf("Today = %s, want the pretend day %s", b.Today, b.AsIf)
		}
		return r.find(ctx, tx, b)
	}}
	var zone string
	if err := db.WithTenant(ctx, app, pl.tenant.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT timezone FROM tenants WHERE id = current_tenant()").Scan(&zone)
	}); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	today = midnight(time.Now().In(loc))
	day := func(n int) string { return today.AddDate(0, 0, n).Format(time.DateOnly) }

	if _, err := c.asIf(ctx, pl.tenant, day(-1)); err == nil || !strings.Contains(err.Error(), "before today") {
		t.Errorf("yesterday = %v, want it refused", err)
	}
	if n, err := c.asIf(ctx, pl.tenant, day(3)); err != nil || n != 2 {
		t.Fatalf("in 3 days = %d, %v; want both items reminded", n, err)
	}
	if lastDay.Format(time.DateOnly) != day(3) {
		t.Errorf("the finder's day was %s, want %s", lastDay, day(3))
	}
	err = db.WithTenant(ctx, app, pl.tenant.ID, func(tx pgx.Tx) error {
		var asIf string
		var at, stored time.Time
		if err := tx.QueryRow(ctx, `SELECT body::jsonb #>> '{payload,change,as_if}', occurred_at, w.reminded_at
			FROM ledger l JOIN work_items w ON w.tenant_id = l.tenant_id AND w.id = l.subject_id
			WHERE l.tenant_id = current_tenant() AND l.subject_id::text = $1 AND l.kind = 'item.posted'
			AND l.body::jsonb #>> '{payload,change,chase}' = 'test'`,
			pl.items["first"]).Scan(&asIf, &at, &stored); err != nil {
			return err
		}
		if asIf != day(3) {
			t.Errorf("event as_if = %q, want %s", asIf, day(3))
		}
		if time.Since(at) > time.Minute || time.Since(stored) > time.Minute {
			t.Errorf("event at %s, reminded_at %s; want real times (now), not the pretend day", at, stored)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.asIf(ctx, pl.tenant, day(2)); err == nil || !strings.Contains(err.Error(), "the last pretend day") {
		t.Errorf("a day before the last pretend day = %v, want it refused", err)
	}
	if n, err := c.asIf(ctx, pl.tenant, day(3)); err != nil || n != 0 {
		t.Errorf("the same pretend day again = %d, %v; want it allowed, with nothing more to do", n, err)
	}
	if n, err := c.asIf(ctx, pl.tenant, day(0)); err == nil {
		t.Errorf("today after a pretend day in 3 days = %d, %v; want it refused", n, err)
	}
	clean(t, app, pl)
}
