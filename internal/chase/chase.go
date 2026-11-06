// SPDX-License-Identifier: AGPL-3.0-or-later

// Package chase is the chaser (D75, D76): a small timer inside the app. Every few minutes it wakes up
// (a round), looks at each business in turn, and does what is due, as Seyal (system). What it did is
// kept on the items themselves, so it is in every ledger snapshot, `seyal verify` checks it, and a
// restart or a second copy of the app never does the same thing twice.
//
// A round first only reads, then does each thing in its own short transaction, so a person's click
// waits a few milliseconds at most. Every read and every action goes through db.WithTenant: one
// business at a time, with row-level security, as the app role.
package chase

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // time zone data built in, so each business's day is right on any server

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seyal/internal/db"
	"seyal/internal/ledger"
)

const (
	DefaultEvery = 5 * time.Minute // how often the chaser wakes when SEYAL_CHASE_EVERY is empty
	MinEvery     = time.Minute     // the shortest gap SEYAL_CHASE_EVERY may set
	FirstWait    = time.Minute     // the first round runs this long after start (a crash loop can't hammer the database)
	ClockSlack   = 2 * time.Minute // how far the clocks may disagree before a round does nothing
	DemoSlug     = "demo"          // the only business `dev chase --as-if` may act for
	logPrefix    = "chase"         // every log line starts with it
	dayLayout    = time.DateOnly   // how a day is written: 2026-11-09
)

// Limits bound one round. The zero value of a field means its default (see DefaultLimits).
type Limits struct {
	LockWait   time.Duration // how long one action waits for the business's ledger lock (Postgres lock_timeout)
	QueryLimit time.Duration // how long one query of an action may run (Postgres statement_timeout)
	Business   time.Duration // how long one business may take in one round
	MaxActions int           // actions that worked, per business per round; the rest wait for the next round
}

// DefaultLimits are the limits of the design (D75, P1 steps 6 and 7).
var DefaultLimits = Limits{LockWait: 5 * time.Second, QueryLimit: 10 * time.Second, Business: 30 * time.Second, MaxActions: 50}

// Business is what a finder knows about the business it looks at.
type Business struct {
	db.Tenant
	Zone     *time.Location // the business's time zone: days are counted in it
	Calendar Calendar       // its working days (days off and holidays), in Zone
	// Today is the business's day (midnight in Zone) for working out what is due. With `dev chase
	// --as-if` it is the pretend day; everything stored stays real (Now).
	Today time.Time
	AsIf  string    // the pretend day ("2026-11-09"), or "" in a real round; events about it add "as_if"
	Now   time.Time // the real moment this business is looked at: what is stored (reminded_at, events) uses it
}

// A Task is one thing that may be due, for example one item to remind. The round runs Do in its own
// short transaction, after taking the business's ledger lock; Do reads the item again, checks it is
// still due (same step stay, not already done), acts, and says what it did in a few words for the log
// ("" when it was no longer due, which isn't counted). Every change Do makes is a ledger event by Seyal
// (system), written with the table change.
type Task struct {
	Ref string    // what the log calls it: an item number, never a person's name
	Key string    // what "skip it for the rest of the day" remembers, e.g. an item id
	Due time.Time // when it became due: the oldest go first
	Do  func(ctx context.Context, tx pgx.Tx) (did string, err error)
}

// A Finder lists what may be due in one business. It only reads: its transaction is read-only.
type Finder func(ctx context.Context, tx pgx.Tx, b Business) ([]Task, error)

// Finders is what a round looks for: reminders and escalations (M6, deadlines.go). Recurring work
// (R) adds its own later.
var Finders = []Finder{findDeadlines}

// Chaser runs rounds. Make one with New; the fields left empty get their defaults.
type Chaser struct {
	DB      db.Beginner // the app pool: the app role, row-level security on
	Log     *log.Logger
	Finders []Finder                                       // nil: Finders
	List    func(ctx context.Context) ([]db.Tenant, error) // the businesses to look at; nil: all of them
	Now     func() time.Time                               // the app's clock; nil: time.Now (tests use another)
	Limits  Limits

	mu     sync.Mutex        // one round at a time
	skip   map[string]string // business id + task key → the day it failed, so it waits until tomorrow
	paused map[string]bool   // businesses whose set-up file says "chase: paused", logged once
}

// New returns a chaser for the app pool that writes its log lines to l.
func New(pool db.Beginner, l *log.Logger) *Chaser {
	return &Chaser{DB: pool, Log: l}
}

// Every reads the SEYAL_CHASE_EVERY setting: empty is DefaultEvery, "off" is 0 (no chasing at all),
// anything else a Go duration such as 5m or 1h, at least MinEvery.
func Every(setting string) (time.Duration, error) {
	switch setting {
	case "":
		return DefaultEvery, nil
	case "off":
		return 0, nil
	}
	d, err := time.ParseDuration(setting)
	if err != nil {
		return 0, fmt.Errorf("SEYAL_CHASE_EVERY=%s: write a time such as 5m or 1h, or off", setting)
	}
	if d < MinEvery {
		return 0, fmt.Errorf("SEYAL_CHASE_EVERY=%s is too short: at least 1m (one minute), or off", setting)
	}
	return d, nil
}

// Says is the line `seyal serve` prints at start about the chaser.
func Says(every time.Duration) string {
	if every <= 0 {
		return "Seyal doesn't chase work on this server (SEYAL_CHASE_EVERY=off)."
	}
	return "Seyal checks for work to chase every " + inWords(every) + "."
}

// inWords writes a gap as people say it: "5 minutes", "1 hour", "90 seconds".
func inWords(d time.Duration) string {
	n, unit := int64(d/time.Second), "second"
	switch {
	case d%time.Hour == 0:
		n, unit = int64(d/time.Hour), "hour"
	case d%time.Minute == 0:
		n, unit = int64(d/time.Minute), "minute"
	}
	if n != 1 {
		unit += "s"
	}
	return strconv.FormatInt(n, 10) + " " + unit
}

// Run runs a round every `every` until ctx ends (Ctrl+C, or the server being stopped). The first round
// runs `first` after start. When ctx ends, a round in progress starts nothing new, and Run returns.
func (c *Chaser) Run(ctx context.Context, every, first time.Duration) {
	wait := time.NewTimer(first)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return
	case <-wait.C:
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		c.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Round looks at every business once and does what is due. It returns how many things it did. A round
// that does nothing writes nothing; the log gets a line for each thing done or failed.
func (c *Chaser) Round(ctx context.Context) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkClock(ctx, c.now()); err != nil {
		c.Log.Printf("%s: %v; this round does nothing", logPrefix, err)
		return 0
	}
	list, err := c.businesses(ctx)
	if err != nil {
		c.Log.Printf("%s: can't list the businesses: %v", logPrefix, err)
		return 0
	}
	done := 0
	for _, t := range list {
		if ctx.Err() != nil {
			break // stopping: start nothing new
		}
		n, err := c.business(ctx, t, "")
		if err != nil && ctx.Err() == nil {
			c.Log.Printf("%s %s: %v", logPrefix, t.Slug, err)
		}
		done += n
	}
	return done
}

// AsIf runs one round for the demo business only, as if today were day (written 2026-11-09), for trying
// things without waiting days (`dev chase --as-if`). Only what is due is worked out from day; every
// stored time stays real, and events add "as_if". It refuses another business, a day before the real
// today, and a day before the last pretend day in the ledger, so time never runs backwards.
func (c *Chaser) AsIf(ctx context.Context, slug, day string) (int, error) {
	if slug != DemoSlug {
		return 0, fmt.Errorf("only the demo business (%q) can be chased as if on another day, not %q", DemoSlug, slug)
	}
	list, err := c.businesses(ctx)
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(list, func(t db.Tenant) bool { return t.Slug == slug })
	if i < 0 {
		return 0, fmt.Errorf("there is no %q business here; load it with \"seyal seed\"", slug)
	}
	return c.asIf(ctx, list[i], day)
}

// asIf is AsIf for any business; only AsIf (the demo only) and tests call it.
func (c *Chaser) asIf(ctx context.Context, t db.Tenant, day string) (int, error) {
	if _, err := time.Parse(dayLayout, day); err != nil {
		return 0, fmt.Errorf("%q: write the day as 2026-11-09", day)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkClock(ctx, c.now()); err != nil {
		return 0, err
	}
	return c.business(ctx, t, day)
}

// business chases one business: it reads what may be due (read-only), then does each thing, oldest
// first, each in its own transaction, within the round's limits. It reads the clock again (a round over
// many businesses takes a while, and a reading from the round's start would grow old) and checks it
// against the database's clock and the business's last ledger event.
func (c *Chaser) business(ctx context.Context, t db.Tenant, asIf string) (int, error) {
	lim := c.limits()
	ctx, cancel := context.WithTimeout(ctx, lim.Business)
	defer cancel()

	now := c.now()
	b := Business{Tenant: t, Now: now, AsIf: asIf}
	var tasks []Task
	paused := false
	err := db.WithTenant(ctx, c.DB, t.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		var zone string
		var last *time.Time
		var dbNow time.Time
		var daysOff []int16
		if err := tx.QueryRow(ctx, `
			SELECT timezone, (SELECT occurred_at FROM ledger WHERE tenant_id = current_tenant() ORDER BY seq DESC LIMIT 1), now(),
				days_off, ARRAY(SELECT to_char(d, 'YYYY-MM-DD') FROM unnest(holidays) d ORDER BY d), chase_paused
			FROM tenants WHERE id = current_tenant()`).Scan(&zone, &last, &dbNow, &daysOff, &b.Calendar.Holidays, &paused); err != nil {
			return fmt.Errorf("business settings: %w", err)
		}
		if paused {
			return nil // its set-up file stops chasing for this business only (decision 20)
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return fmt.Errorf("time zone %q: %w", zone, err)
		}
		b.Calendar.Zone = loc
		for _, d := range daysOff {
			b.Calendar.DaysOff = append(b.Calendar.DaysOff, time.Weekday(d))
		}
		if last != nil {
			if err := notBefore(now, *last); err != nil {
				return err
			}
		}
		if err := clocksAgree(now, dbNow); err != nil {
			return fmt.Errorf("%w; nothing done", err)
		}
		b.Zone, b.Today = loc, midnight(now.In(loc))
		if asIf != "" {
			if b.Today, err = pretendDay(ctx, tx, asIf, loc, b.Today); err != nil {
				return err
			}
		}
		for _, find := range c.finders() {
			found, err := safely(ctx, tx, b, find)
			if err != nil {
				return err
			}
			tasks = append(tasks, found...)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if c.notePaused(t, paused) {
		return 0, nil
	}

	slices.SortStableFunc(tasks, func(x, y Task) int { return x.Due.Compare(y.Due) })
	today := now.In(b.Zone).Format(dayLayout) // the real day: a failed item waits until the real tomorrow
	done := 0
	for _, task := range tasks {
		if done >= lim.MaxActions || ctx.Err() != nil {
			break // the rest wait for the next round
		}
		key := t.ID + " " + task.Key
		if c.skip[key] == today {
			continue
		}
		did, err := c.do(ctx, t.ID, task, lim)
		switch {
		case err == nil && did != "":
			done++
			c.Log.Printf("%s %s: %s: %s", logPrefix, t.Slug, task.Ref, did)
		case err == nil:
			// no longer due (someone moved it, or another copy of the app did it first)
		case busy(err):
			c.Log.Printf("%s %s: %s: busy, tried again next round", logPrefix, t.Slug, task.Ref)
		case ctx.Err() != nil:
			// the business's time is up, or the server is stopping: not the item's fault
		default:
			c.forget(t.ID, today)
			c.remember(key, today)
			c.Log.Printf("%s %s: %s: failed, skipped until tomorrow: %v", logPrefix, t.Slug, task.Ref, err)
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		c.Log.Printf("%s %s: stopped after %s; the rest wait for the next round", logPrefix, t.Slug, lim.Business)
	}
	return done, nil
}

// do runs one task in its own transaction: time limits first, then the business's ledger lock (the lock
// every write takes first), then the task. A panic in the task is caught here, so the tasks after it
// still run; Postgres undoes the transaction.
func (c *Chaser) do(ctx context.Context, tenantID string, task Task, lim Limits) (did string, err error) {
	defer func() {
		if r := recover(); r != nil {
			did, err = "", fmt.Errorf("panic: %v", r)
		}
	}()
	err = db.WithTenant(ctx, c.DB, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)",
			millis(lim.LockWait), millis(lim.QueryLimit)); err != nil {
			return err
		}
		if err := ledger.Lock(ctx, tx); err != nil {
			return err
		}
		var err error
		did, err = task.Do(ctx, tx)
		return err
	})
	if err != nil {
		did = ""
	}
	return did, err
}

// safely runs one finder. A panic in it (a bug while reading one item) becomes this business's error,
// so the round goes on with the next business and the web server keeps running; Postgres undoes the
// read-only transaction.
func safely(ctx context.Context, tx pgx.Tx, b Business, find Finder) (tasks []Task, err error) {
	defer func() {
		if r := recover(); r != nil {
			tasks, err = nil, fmt.Errorf("panic while looking for work: %v", r)
		}
	}()
	return find(ctx, tx, b)
}

// checkClock compares the app's clock with the database's.
func (c *Chaser) checkClock(ctx context.Context, now time.Time) error {
	var dbNow time.Time
	err := db.WithServer(ctx, c.DB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT now()").Scan(&dbNow)
	})
	if err != nil {
		return fmt.Errorf("can't reach the database: %w", err)
	}
	return clocksAgree(now, dbNow)
}

// clocksAgree refuses an app clock more than ClockSlack away from the database's: a clock that jumps
// can't make work look days late.
func clocksAgree(app, database time.Time) error {
	if d := app.Sub(database); d > ClockSlack || d < -ClockSlack {
		return fmt.Errorf("this server's clock is %s away from the database's clock", d.Abs().Round(time.Second))
	}
	return nil
}

// notBefore refuses an app clock more than ClockSlack earlier than the business's last ledger event: a
// clock that goes back can't cause a second reminder.
func notBefore(app, lastEvent time.Time) error {
	if d := lastEvent.Sub(app); d > ClockSlack {
		return fmt.Errorf("this server's clock is %s behind the last ledger event; nothing done", d.Round(time.Second))
	}
	return nil
}

// pretendDay checks a `--as-if` day: not before the real today, and not before the last pretend day in
// the ledger. It returns the day as midnight in loc.
func pretendDay(ctx context.Context, tx pgx.Tx, day string, loc *time.Location, today time.Time) (time.Time, error) {
	d, err := time.ParseInLocation(dayLayout, day, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: write the day as 2026-11-09", day)
	}
	if d.Before(today) {
		return time.Time{}, fmt.Errorf("%s is before today (%s): time can't run backwards", day, today.Format(dayLayout))
	}
	var last string
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(max(coalesce(body::jsonb #>> '{payload,change,as_if}', body::jsonb #>> '{payload,as_if}')), '')
		FROM ledger WHERE tenant_id = current_tenant() AND actor_kind = 'system'`).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("last pretend day: %w", err)
	}
	if day < last {
		return time.Time{}, fmt.Errorf("%s is before %s, the last pretend day in the ledger: time can't run backwards", day, last)
	}
	return d, nil
}

// busy reports whether err is Postgres giving up waiting for a lock (lock_timeout): someone else is
// writing to the business, so the task is tried again next round.
func busy(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03" // lock_not_available
}

// notePaused logs once that a business is paused (its set-up file says "chase: paused"), and again
// only after it was chased in between. It returns paused.
func (c *Chaser) notePaused(t db.Tenant, paused bool) bool {
	if c.paused == nil {
		c.paused = map[string]bool{}
	}
	if paused && !c.paused[t.ID] {
		c.Log.Printf("%s %s: paused in its set-up file (chase: paused); nothing is chased there", logPrefix, t.Slug)
	}
	c.paused[t.ID] = paused
	return paused
}

// remember keeps key as failed today; forget drops what one business failed on earlier days.
func (c *Chaser) remember(key, today string) {
	if c.skip == nil {
		c.skip = map[string]string{}
	}
	c.skip[key] = today
}

func (c *Chaser) forget(tenantID, today string) {
	for key, day := range c.skip {
		if day != today && strings.HasPrefix(key, tenantID+" ") {
			delete(c.skip, key)
		}
	}
}

func (c *Chaser) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Chaser) finders() []Finder {
	if c.Finders != nil {
		return c.Finders
	}
	return Finders
}

func (c *Chaser) businesses(ctx context.Context) ([]db.Tenant, error) {
	if c.List != nil {
		return c.List(ctx)
	}
	return db.Tenants(ctx, c.DB)
}

func (c *Chaser) limits() Limits {
	l := c.Limits
	return Limits{
		LockWait:   cmp.Or(l.LockWait, DefaultLimits.LockWait),
		QueryLimit: cmp.Or(l.QueryLimit, DefaultLimits.QueryLimit),
		Business:   cmp.Or(l.Business, DefaultLimits.Business),
		MaxActions: cmp.Or(l.MaxActions, DefaultLimits.MaxActions),
	}
}

// midnight is the start of t's day, in t's time zone.
func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// millis writes a time limit as Postgres reads it: "5000ms".
func millis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}
