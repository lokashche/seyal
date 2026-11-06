//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package notify_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
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
	"seyal/internal/notify"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/push"
	"seyal/internal/send"
	"seyal/internal/setup"
	"seyal/internal/testdb"
	"seyal/internal/workitem"
)

// fakeOut stands in for the push services (it does what *push.Sender does, without locking the note
// or calling anyone): it keeps the phone notes posted to it, as the phone's service worker would read
// them, and answers 201 unless a test says otherwise. Nothing leaves this computer.
type fakeOut struct {
	mu    sync.Mutex
	notes []note
	posts int // every post, whatever the answer
	fail  int // answer this many posts first with "could not connect", as a push service that is down would
	calls int
	// before, when set, runs first on each Send, with its number (1, 2, ...): a test's way to make
	// the sender slow, or to make it break.
	before func(call int)
	// answer, when set, gives the reply code (and Retry-After) for each post; nil means 201.
	answer func(d push.Device) (status int, retryAfter string)
	// err, when set, is returned instead of an answer (a broken sender).
	err error
}

// note is one phone note a device got: who owns the device, and what its service worker reads.
type note struct {
	Person   string // the device owner's name
	Endpoint string
	Body     string
	Navigate string
	Tag      string
	TTL      time.Duration
}

// deviceOwner is the name of each test device's owner, by device push address. It is shared by every
// firm: in TestTwoBusinesses Dev's one login (and device) serves two businesses.
var deviceOwner sync.Map

// whenSending sets before, counting the Sends from now on.
func (f *fakeOut) whenSending(before func(call int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.before = 0, before
}

// answering sets answer.
func (f *fakeOut) answering(answer func(d push.Device) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeOut) Send(_ context.Context, d push.Device, m push.Message) (send.Answer, error) {
	f.mu.Lock()
	f.calls++
	call, before := f.calls, f.before
	f.mu.Unlock()
	if before != nil {
		before(call)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	if f.err != nil {
		return send.Answer{}, f.err
	}
	if f.fail > 0 {
		f.fail--
		return send.Answer{}, &send.Failure{Reason: send.ReasonConnect}
	}
	status, retry := http.StatusCreated, ""
	if f.answer != nil {
		status, retry = f.answer(d)
	}
	if status/100 != 2 {
		return send.Answer{Status: status, RetryAfter: retry}, nil
	}
	var p struct {
		WebPush      int `json:"web_push"`
		Notification struct {
			Title, Body, Navigate, Tag string
		} `json:"notification"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil || p.WebPush != 8030 || p.Notification.Title != notify.Title ||
		p.Notification.Tag != m.Topic {
		return send.Answer{}, fmt.Errorf("the fake push service can't read the note %s: %v", m.Payload, err)
	}
	owner, _ := deviceOwner.Load(d.Endpoint)
	name, _ := owner.(string)
	f.notes = append(f.notes, note{Person: name, Endpoint: d.Endpoint, Body: p.Notification.Body, Navigate: p.Notification.Navigate,
		Tag: p.Notification.Tag, TTL: m.TTL})
	return send.Answer{Status: status}, nil
}

// addDevice turns phone notes on for the person with key in a made-up browser, as the owner (the way
// TurnOnDevice would), and returns its device push address and id.
func (f *firm) addDevice(key string) (endpoint, id string) {
	f.t.Helper()
	return f.addDeviceAt(key, "https://fcm.googleapis.com/fcm/send/")
}

// addDeviceAt is addDevice with a device push address under base, a made-up one of another push
// service (base names the service; nothing is ever posted there).
func (f *firm) addDeviceAt(key, base string) (endpoint, id string) {
	f.t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	endpoint = base + "test-" + key + "-" + rand.Text()
	session := fmt.Sprintf("%x", sha256.Sum256([]byte(endpoint)))
	err = db.WithTenant(f.ctx, f.owner, f.tenant.ID, func(tx pgx.Tx) error { // persons' row rule binds the owner too
		return tx.QueryRow(f.ctx, `
			WITH a AS (SELECT account_id FROM persons WHERE id = $1),
			s AS (INSERT INTO account_sessions (token_sha256, account_id, expires_at)
				SELECT $2, account_id, now() + interval '14 days' FROM a RETURNING account_id)
			INSERT INTO push_devices (account_id, account_session_id, endpoint, p256dh, auth, origin, label)
			SELECT account_id, $2, $3, $4, $5, 'https://seyal.kaveri-demo.example', 'Chrome on Android' FROM s
			RETURNING id::text`, f.people[key], session, endpoint,
			base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)).Scan(&id)
	})
	if err != nil {
		f.t.Fatalf("a device for %s: %v", key, err)
	}
	deviceOwner.Store(endpoint, f.names[f.people[key]])
	return endpoint, id
}

// endedWhy is why a device ended ("" while it gets phone notes), read as the owner.
func (f *firm) endedWhy(id string) string {
	f.t.Helper()
	var why string
	if err := f.owner.QueryRow(f.ctx, `SELECT coalesce(ended_why, '') FROM push_devices WHERE id = $1`, id).Scan(&why); err != nil {
		f.t.Fatal(err)
	}
	return why
}

// firm is a fresh copy of Kaveri Associates (demo) on the shipped samples (all made up). Asha (Sales),
// Ben and Dev (Operations), Chitra (Owner, admin) and Elan (Owner) have logins; the others don't, so
// they are never told. Phone notes are on, and unless a test says otherwise every hour of every day
// is a working hour, so only the quiet-hours tests depend on the clock.
type firm struct {
	t      *testing.T
	ctx    context.Context
	owner  *pgxpool.Pool
	app    *pgxpool.Pool
	tenant db.Tenant
	people map[string]string // demo key → person id
	names  map[string]string // person id → name
	// devices and endpoints are each person's phone, by demo key: its id and its device push address.
	// Asha, Ben, Dev, Chitra and Elan each have one.
	devices, endpoints map[string]string
	job                string // job evaluation's version id
	zone               *time.Location
	clock              time.Time // the reader's clock
	out                *fakeOut
	log                strings.Builder
	r                  *notify.Reader
}

func newFirm(t *testing.T) *firm {
	t.Helper()
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := setup.Load("../../examples/seed/demo.yaml", lib)
	if err != nil {
		t.Fatal(err)
	}
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10])
	for i, p := range f.People {
		f.People[i].Email = p.Key + "@" + f.Business.Slug + ".kaveri-demo.example"
	}
	sum, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	fm := &firm{t: t, ctx: ctx, owner: owner, app: app, tenant: db.Tenant{ID: sum.TenantID, Slug: f.Business.Slug},
		people: sum.People, names: map[string]string{}, clock: time.Now(), out: &fakeOut{}}
	for _, key := range []string{"asha", "ben", "dev", "chitra", "elan"} {
		testdb.GiveLogins(t, owner, sum.TenantID, fm.people[key])
	}
	for _, p := range f.People {
		fm.names[sum.People[p.Key]] = p.Name
	}
	fm.devices, fm.endpoints = map[string]string{}, map[string]string{}
	for _, key := range []string{"asha", "ben", "dev", "chitra", "elan"} {
		fm.endpoints[key], fm.devices[key] = fm.addDevice(key) // each has one phone with phone notes on
	}
	fm.do(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'job-evaluation'`).Scan(&fm.job)
	})
	if fm.zone, err = time.LoadLocation("Asia/Kolkata"); err != nil {
		t.Fatal(err)
	}
	fm.settings("notify_from = '00:00', notify_until = '24:00', days_off = '{}', holidays = '{}'")
	fm.r = &notify.Reader{DB: app, Log: log.New(&fm.log, "", 0), Push: true, Out: fm.out,
		List: func(context.Context) ([]db.Tenant, error) { return []db.Tenant{fm.tenant}, nil },
		Now:  func() time.Time { return fm.clock }}
	fm.pass() // the bookmark starts here: setting the business up tells nobody
	return fm
}

// settings changes the business's settings as the set-up import would (the app may only read them).
func (f *firm) settings(set string) {
	f.t.Helper()
	if _, err := f.owner.Exec(f.ctx, "UPDATE tenants SET "+set+" WHERE id = $1", f.tenant.ID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *firm) do(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := db.WithTenant(f.ctx, f.app, f.tenant.ID, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *firm) by(key string) ledger.Actor { return ledger.PersonActor(f.people[key]) }

// pass runs the reader once and returns what each person was told, in plain words: "Dev: New work
// from Ben: J-2026-0001", sorted.
func (f *firm) pass() []string {
	f.t.Helper()
	f.r.Pass(f.ctx)
	if f.log.Len() > 0 {
		f.t.Errorf("the reader logged: %s", f.log.String())
		f.log.Reset()
	}
	return f.told()
}

// told returns, and forgets, what the fake sender was handed.
func (f *firm) told() []string {
	f.out.mu.Lock()
	defer f.out.mu.Unlock()
	var out []string
	for _, n := range f.out.notes {
		out = append(out, n.Person+": "+n.Body)
	}
	f.out.notes = nil
	slices.Sort(out)
	return out
}

func (f *firm) notes() []note {
	f.out.mu.Lock()
	defer f.out.mu.Unlock()
	return slices.Clone(f.out.notes)
}

// want checks what each person was told in one step of a story. With -v the steps are printed, in
// plain words.
func want(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
	if len(got) == 0 {
		t.Logf("%s: nobody's phone buzzes", what)
	}
	for _, line := range got {
		t.Logf("%s: %s", what, line)
	}
}

// verified checks that the ledger's chain is whole and the tables agree with it (seyal verify):
// phone notes add no ledger events, and a pause adds a business event only.
func (f *firm) verified() {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		rep, err := ledger.Verify(f.ctx, tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			f.t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			f.t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}

// startJob starts a job as Asha for Sundaram Textiles' monthly books and sends it to Operations.
func (f *firm) startJob() workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: f.job, Via: "chat", By: f.by("asha"),
			Fields: engine.Fields{"client": "sundaram-textiles", "nature_of_work": "monthly-books", "reason": "Books for the year",
				"integrity_check": "pass", "fee_agreed": 1000000.0}})
		if err != nil {
			return err
		}
		it, err = workitem.Act(f.ctx, tx, it.ID, "send_to_ops", "", f.by("asha"))
		return err
	})
	return it
}

// heldBy starts n jobs that Ben has taken, before the reader looks (nobody is told about them).
func (f *firm) heldBy(n int, key string) []workitem.Item {
	f.t.Helper()
	var items []workitem.Item
	for range n {
		it := f.startJob()
		items = append(items, f.take(it.ID, key))
	}
	f.pass()
	return items
}

func (f *firm) take(id, key string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Take(f.ctx, tx, id, "", "panel", f.by(key))
		return err
	})
	return it
}

func (f *firm) passOn(id, from, to string) {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		_, _, err := workitem.Pass(f.ctx, tx, id, workitem.PassOn{Person: f.people[to], Via: "button"}, f.by(from))
		return err
	})
}

// results counts the business's notices by what became of them ("" while waiting).
func (f *firm) results() map[string]int {
	f.t.Helper()
	out := map[string]int{}
	f.do(func(tx pgx.Tx) error {
		rows, _ := tx.Query(f.ctx, `SELECT coalesce(result, ''), count(*) FROM notices WHERE tenant_id = current_tenant() GROUP BY 1`)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			R string
			N int
		}])
		for _, r := range list {
			out[r.R] = r.N
		}
		return err
	})
	return out
}

// TestTheWeek follows the design's story (section 2.1, "Example") with real changes and the real
// ledger: who is told, and never the person who acted.
func TestTheWeek(t *testing.T) {
	f := newFirm(t)
	job := f.startJob()
	want(t, "Asha sends a new job to Operations", f.pass(),
		"Ben: New work for Operations from Asha: "+job.Number, "Dev: New work for Operations from Asha: "+job.Number)

	f.take(job.ID, "ben")
	want(t, "Ben takes it himself", f.pass())

	f.passOn(job.ID, "ben", "dev")
	want(t, "Ben passes it to Dev", f.pass(), "Dev: New work from Ben: "+job.Number)
	if n := f.notes(); len(n) != 0 {
		t.Errorf("notes left over: %v", n)
	}

	f.do(func(tx pgx.Tx) error {
		_, err := workitem.Act(f.ctx, tx, job.ID, "submit", "", f.by("dev"))
		return err
	})
	var st engine.ApprovalStatus
	f.do(func(tx pgx.Tx) error {
		var err error
		st, err = workitem.Approvals(f.ctx, tx, job.ID)
		return err
	})
	wanted := []string{"Ben: Your approval is needed: " + job.Number, "Chitra: Your approval is needed: " + job.Number,
		"Elan: Your approval is needed: " + job.Number}
	if slices.Contains(st.Waiting, "sales") {
		wanted = append(wanted, "Asha: Your approval is needed: "+job.Number)
	}
	if !slices.Contains(st.Waiting, "owner") || !slices.Contains(st.Waiting, "ops") {
		t.Fatalf("waiting for %v; the test wants Ops and Owner", st.Waiting)
	}
	// Dev submitted: he is told nothing, though he may approve the Ops slot himself (decision 6).
	want(t, "Dev submits", f.pass(), wanted...)

	f.do(func(tx pgx.Tx) error {
		_, err := workitem.Decide(f.ctx, tx, job.ID, f.people["ben"], "ops", engine.Approve, "")
		return err
	})
	want(t, "Ben approves for Operations", f.pass())
	f.verified()
}

// TestDropped: a phone note waiting to go is dropped when it no longer matters.
func TestDropped(t *testing.T) {
	f := newFirm(t)
	items := f.heldBy(3, "ben")
	read := func() {
		t.Helper()
		if _, err := f.r.Read(f.ctx, f.tenant); err != nil {
			t.Fatal(err)
		}
	}
	deliver := func() []string {
		t.Helper()
		if _, err := f.r.Deliver(f.ctx, f.tenant); err != nil {
			t.Fatal(err)
		}
		return f.told()
	}

	// Dev opens the item after Ben passed it, before the phone note went.
	f.passOn(items[0].ID, "ben", "dev")
	read()
	f.do(func(tx pgx.Tx) error { return workitem.MarkSeen(f.ctx, tx, f.people["dev"], items[0].ID, time.Now()) })
	want(t, "Dev opened it first", deliver())

	// The item is cancelled before the phone note went.
	f.passOn(items[1].ID, "ben", "dev")
	read()
	f.do(func(tx pgx.Tx) error {
		_, err := workitem.Act(f.ctx, tx, items[1].ID, "cancel", "The client stopped", f.by("dev"))
		return err
	})
	want(t, "cancelled first", deliver())

	// Chitra approves the Owner slot before Elan's phone note goes: Elan's is dropped (and Chitra's).
	f.passOn(items[2].ID, "ben", "dev")
	f.pass()
	f.do(func(tx pgx.Tx) error {
		_, err := workitem.Act(f.ctx, tx, items[2].ID, "submit", "", f.by("dev"))
		return err
	})
	read()
	f.do(func(tx pgx.Tx) error {
		_, err := workitem.Decide(f.ctx, tx, items[2].ID, f.people["chitra"], "owner", engine.Approve, "")
		return err
	})
	got := deliver()
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "Elan:") || strings.HasPrefix(s, "Chitra:") }) ||
		!slices.Contains(got, "Ben: Your approval is needed: "+items[2].Number) {
		t.Errorf("after Chitra approved for the Owners: %q; want Ben's only (and Asha's, if Sales must approve)", got)
	}
	if r := f.results(); r[notify.Seen] != 1 || r[notify.Closed] != 1 || r[notify.Decided] != 2 || r[""] != 0 {
		t.Errorf("notices by result: %v; want 1 seen, 1 closed, 2 decided, none waiting", r)
	}
}

// TestBulkMoveIsOneNote: Chitra moves 30 of Ben's items to Dev in one go. Dev gets one phone note;
// Ben and Chitra get none.
func TestBulkMoveIsOneNote(t *testing.T) {
	f := newFirm(t)
	items := f.heldBy(30, "ben")
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	f.do(func(tx pgx.Tx) error {
		moved, err := workitem.Reassign(f.ctx, tx, ids, f.people["ben"], f.people["dev"], "", f.by("chitra"))
		if err == nil && len(moved.Moved) != 30 {
			err = fmt.Errorf("moved %d, skipped %v", len(moved.Moved), moved.Skipped)
		}
		return err
	})
	want(t, "30 items moved", f.pass(), "Dev: 30 items moved to you by Chitra")
	f.do(func(tx pgx.Tx) error {
		var n, notes int
		err := tx.QueryRow(f.ctx, `SELECT count(*), count(DISTINCT note_id) FROM notices
			WHERE tenant_id = current_tenant() AND batch <> '' AND result = 'sent'`).Scan(&n, &notes)
		if n != 30 || notes != 1 {
			t.Errorf("%d notices of the bulk move sent, in %d phone notes; want 30 in one", n, notes)
		}
		return err
	})
	want(t, "the next pass", f.pass())

	// Three hand-overs, one by one, before the next pass: one grouped phone note.
	for _, it := range items[:3] {
		f.passOn(it.ID, "dev", "ben")
	}
	want(t, "three hand-overs in one pass", f.pass(), "Ben: 3 new things for you")
}

// TestHourlyCap: at most 20 phone notes per person per hour; the rest wait and go as one.
func TestHourlyCap(t *testing.T) {
	f := newFirm(t)
	items := f.heldBy(23, "ben") // Ben and Dev get one grouped note about the 23 new jobs
	start := f.clock.Add(2 * time.Hour)
	for i, it := range items {
		f.clock = start.Add(time.Duration(i) * time.Minute)
		f.passOn(it.ID, "ben", "dev")
		got := f.pass()
		if i < notify.MaxPerHour && len(got) != 1 || i >= notify.MaxPerHour && len(got) != 0 {
			t.Fatalf("hand-over %d: told %q", i+1, got)
		}
	}
	f.clock = start.Add(time.Hour + time.Minute) // the first one is more than an hour old
	want(t, "the next hour", f.pass(), "Dev: 3 new things for you")
}

// TestNightReminder: the chaser's reminder at 00:05 waits for the 09:00 morning note; so do 12
// pieces of recurring work Seyal starts at 00:05.
func TestNightReminder(t *testing.T) {
	f := newFirm(t)
	job := f.heldBy(1, "dev")[0]
	f.settings("notify_from = '09:00', notify_until = '19:00'")
	day := time.Now().In(f.zone)
	at := func(hh, mm int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, f.zone)
	}

	f.clock = at(0, 5)
	f.do(func(tx pgx.Tx) error {
		if err := ledger.Lock(f.ctx, tx); err != nil {
			return err
		}
		did, err := workitem.Remind(f.ctx, tx, job.ID, workitem.Chase{StepSince: job.StepSince, Days: 2, Now: time.Now(), Zone: f.zone})
		if err == nil && did == "" {
			err = fmt.Errorf("no reminder written")
		}
		return err
	})
	want(t, "00:05", f.pass())
	f.clock = at(8, 59)
	want(t, "08:59", f.pass())
	f.clock = at(9, 0)
	want(t, "09:00", f.pass(), "Dev: Reminder: "+job.Number+" is waiting on you")
	if n := f.notes(); len(n) != 0 {
		t.Errorf("left over: %v", n)
	}

	// Recurring work: Seyal starts 12 monthly returns at 00:05; Dev handles Sundaram Textiles.
	var monthly string
	f.do(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'monthly-return'`).Scan(&monthly)
	})
	f.clock = at(0, 5)
	for i := range 12 {
		f.do(func(tx pgx.Tx) error {
			_, err := workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: monthly, Via: "schedule",
				Ref: fmt.Sprintf("test-run-%d-%s", i, rand.Text()[:6]), By: ledger.SystemActor, Period: "Oct 2026",
				Fields: engine.Fields{"client": "sundaram-textiles", "period": "Oct 2026"}})
			return err
		})
	}
	want(t, "00:05, 12 returns started", f.pass())
	f.clock = at(9, 0).Add(time.Minute)
	got := f.pass()
	want(t, "09:01", got, "Dev: While it was quiet: 12 new work")
}

// TestDaysOffAndHolidays: a hand-over on a holiday waits for the next working day's morning.
func TestDaysOffAndHolidays(t *testing.T) {
	f := newFirm(t)
	day := time.Now().In(f.zone)
	today := day.Format(time.DateOnly)
	tomorrow := time.Date(day.Year(), day.Month(), day.Day()+1, 9, 0, 0, 0, f.zone)
	job := f.heldBy(1, "ben")[0]
	f.settings(fmt.Sprintf("notify_from = '09:00', notify_until = '19:00', holidays = ARRAY['%s']::date[]", today))
	f.clock = time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, f.zone)
	f.passOn(job.ID, "ben", "dev")
	want(t, "10:00 on a holiday", f.pass())
	f.clock = tomorrow.Add(-time.Minute)
	want(t, "08:59 the next day", f.pass())
	f.clock = tomorrow
	want(t, "09:00 the next day", f.pass(), "Dev: New work from Ben: "+job.Number)
}

// TestOffThenOn: while phone notes are off (the server's switch, the set-up file, an admin's pause)
// the bookmark moves on, so switching back on brings nothing about that time. A server that was down
// for an hour still sends that hour's phone notes; one down for three days sends none of them.
func TestOffThenOn(t *testing.T) {
	f := newFirm(t)
	items := f.heldBy(5, "ben")
	for i, off := range []struct {
		name    string
		off, on func()
	}{
		{name: "the server's switch", off: func() { f.r.Push = false }, on: func() { f.r.Push = true }},
		{name: "the set-up file", off: func() { f.settings("notifications = false") }, on: func() { f.settings("notifications = true") }},
		{name: "an admin's pause", off: func() { f.pause(true) }, on: func() { f.pause(false) }},
	} {
		off.off()
		f.passOn(items[i].ID, "ben", "dev")
		f.clock = f.clock.Add(72 * time.Hour) // off for three days
		want(t, off.name+": off", f.pass())
		off.on()
		want(t, off.name+": on again", f.pass())
	}

	// The server is down for an hour in working hours: the hand-over still goes when it is back.
	f.clock = time.Now()
	f.passOn(items[3].ID, "ben", "dev")
	f.clock = time.Now().Add(time.Hour)
	want(t, "back after an hour", f.pass(), "Dev: New work from Ben: "+items[3].Number)

	// Down for three days: the hand-over is too old to tell anyone about.
	f.passOn(items[4].ID, "ben", "dev")
	f.clock = time.Now().Add(72 * time.Hour)
	want(t, "back after three days", f.pass())
	if r := f.results(); r[""] != 0 {
		t.Errorf("notices waiting: %v", r)
	}
	f.verified()
}

// pause presses Pause (or Resume) as Chitra.
func (f *firm) pause(on bool) {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		changed, err := notify.Pause(f.ctx, tx, on, f.people["chitra"])
		if err == nil && !changed {
			err = fmt.Errorf("pause %v changed nothing", on)
		}
		return err
	})
}

// TestPauseIsALedgerEvent: each press is a business.updated event by the admin; pressing twice changes
// nothing the second time.
func TestPauseIsALedgerEvent(t *testing.T) {
	f := newFirm(t)
	f.pause(true)
	f.do(func(tx pgx.Tx) error {
		if changed, err := notify.Pause(f.ctx, tx, true, f.people["chitra"]); err != nil || changed {
			t.Errorf("a second Pause = %v, %v; want no change", changed, err)
		}
		s, err := notify.LoadSettings(f.ctx, tx)
		if err != nil || !s.Paused() || s.PausedBy != f.people["chitra"] {
			t.Errorf("settings after Pause = %+v, %v", s, err)
		}
		return err
	})
	f.pause(false)
	f.do(func(tx pgx.Tx) error {
		rows, _ := tx.Query(f.ctx, `SELECT coalesce(actor_person_id::text, ''), body::jsonb #>> '{payload,changes,phone_notes_paused,new}'
			FROM ledger WHERE tenant_id = current_tenant() AND kind = 'business.updated' AND body::jsonb #> '{payload,changes}' ? 'phone_notes_paused'
			ORDER BY seq`)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ By, New string }])
		if err != nil {
			return err
		}
		if len(list) != 2 || list[0] != (struct{ By, New string }{f.people["chitra"], "true"}) || list[1].New != "false" {
			t.Errorf("pause events: %v; want Chitra's pause, then her resume", list)
		}
		return nil
	})
}

// TestTwoBusinesses: Dev's one login belongs to two businesses. While he is active in both, each phone
// note names its business. Switched off in one, he hears nothing more from it, and still hears from
// the other, without the name (he is active in one business now).
func TestTwoBusinesses(t *testing.T) {
	a, b := newFirm(t), newFirm(t)
	var account string
	a.do(func(tx pgx.Tx) error {
		return tx.QueryRow(a.ctx, `SELECT account_id::text FROM persons WHERE id::text = $1`, a.people["dev"]).Scan(&account)
	})
	// b's Dev already has a login of his own: link b's Dev to a's login instead.
	if err := db.WithTenant(b.ctx, b.owner, b.tenant.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(b.ctx, `UPDATE persons SET account_id = NULL, login_email = NULL WHERE id::text = $1`, b.people["dev"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	testdb.LinkPerson(t, b.owner, b.tenant.ID, b.people["dev"], account)
	b.settings("name = 'Second Office (demo)'")
	ja, jb := a.heldBy(2, "ben"), b.heldBy(2, "ben")

	a.passOn(ja[0].ID, "ben", "dev")
	b.passOn(jb[0].ID, "ben", "dev")
	want(t, "in both", append(a.pass(), b.pass()...),
		"Dev: Kaveri Associates (demo) · New work from Ben: "+ja[0].Number, "Dev: Second Office (demo) · New work from Ben: "+jb[0].Number)

	a.passOn(ja[1].ID, "ben", "dev")
	if _, err := a.r.Read(a.ctx, a.tenant); err != nil { // the phone note is made, and waits
		t.Fatal(err)
	}
	a.do(func(tx pgx.Tx) error {
		_, err := people.SetActive(a.ctx, tx, a.people["dev"], false, a.by("chitra"))
		return err
	})
	b.passOn(jb[1].ID, "ben", "dev")
	want(t, "switched off in the first", append(a.pass(), b.pass()...), "Dev: New work from Ben: "+jb[1].Number)
	if r := a.results(); r[notify.Inactive] != 1 {
		t.Errorf("the first business's notices: %v; want the waiting one dropped (inactive)", r)
	}
}

// lastSeq returns where the reader's bookmark is, and the ledger's end.
func (f *firm) lastSeq() (last, head int64) {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT (SELECT last_seq FROM notice_bookmark WHERE tenant_id = current_tenant()),
			(SELECT max(seq) FROM ledger WHERE tenant_id = current_tenant())`).Scan(&last, &head)
	})
	return last, head
}

// TestExactlyFullPages: when the new events fill whole pages exactly (500, 1000, ...), the bookmark
// still moves to the ledger's end, so the next pass doesn't read them again.
func TestExactlyFullPages(t *testing.T) {
	f := newFirm(t)
	for _, pages := range []int{1, 2} {
		f.do(func(tx pgx.Tx) error {
			for range pages * notify.PageSize {
				if _, err := ledger.Append(f.ctx, tx, ledger.Event{Kind: "test.filler", SubjectType: "test"}.By(f.by("chitra"))); err != nil {
					return err
				}
			}
			return nil
		})
		want(t, fmt.Sprintf("%d full pages of other events", pages), f.pass())
		if last, head := f.lastSeq(); last != head {
			t.Errorf("after %d full pages: the bookmark is at %d, the ledger's end at %d", pages, last, head)
		}
	}
}

// TestNoNoteAtNight: notices that wait past the end of working hours (the hourly cap kept them, or the
// server stopped before they went) don't buzz a phone in the evening. They go in the next morning's note.
func TestNoNoteAtNight(t *testing.T) {
	f := newFirm(t)
	now := time.Now().In(f.zone)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, f.zone)
	if day.Add(17*time.Hour + 50*time.Minute).After(now) {
		day = day.AddDate(0, 0, -1) // the reader's clock stays before the events' real time, so none is too old
	}
	at := func(days, hh, mm int) time.Time {
		return day.AddDate(0, 0, days).Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
	}

	f.clock = at(0, 14, 0) // the first notes about the new jobs go well before the hour that counts
	items := f.heldBy(23, "ben")
	f.settings("notify_from = '09:00', notify_until = '19:00'")
	for i, it := range items {
		f.clock = at(0, 17, 50+i) // 17:50 to 18:12
		f.passOn(it.ID, "ben", "dev")
		got := f.pass()
		if i < notify.MaxPerHour && len(got) != 1 || i >= notify.MaxPerHour && len(got) != 0 {
			t.Fatalf("hand-over %d: told %q", i+1, got)
		}
	}
	// At 18:59 the reader makes Ben's notice, and the server stops before it goes.
	f.clock = at(0, 18, 59)
	f.passOn(items[0].ID, "dev", "ben")
	if _, err := f.r.Read(f.ctx, f.tenant); err != nil {
		t.Fatal(err)
	}

	f.clock = at(0, 19, 5) // the hour has room for Dev again, but it is evening
	want(t, "19:05", f.pass())
	f.clock = at(0, 21, 0) // the server is back
	want(t, "21:00", f.pass())
	f.clock = at(1, 8, 59)
	want(t, "08:59 the next day", f.pass())
	f.clock = at(1, 9, 0)
	want(t, "09:00 the next day", f.pass(), "Dev: While it was quiet: 3 new work", "Ben: New work from Dev: "+items[0].Number)
}

// TestRefusedNoteWaits: a phone note no push service takes is not marked sent. It waits a minute, and
// goes on the first pass after that.
func TestRefusedNoteWaits(t *testing.T) {
	f := newFirm(t)
	job := f.heldBy(1, "ben")[0]
	f.passOn(job.ID, "ben", "dev")
	f.out.fail = 1
	f.r.Pass(f.ctx)
	if !strings.Contains(f.log.String(), "waits") {
		t.Errorf("the reader logged %q; want a line that the phone note waits", f.log.String())
	}
	f.log.Reset()
	if got := f.told(); len(got) != 0 {
		t.Errorf("told %q while the sender was down", got)
	}
	if r := f.results(); r[""] != 1 {
		t.Errorf("notices by result: %v; want Dev's one waiting", r)
	}
	want(t, "the next pass, within the minute", f.pass())
	f.clock = f.clock.Add(time.Minute)
	want(t, "a minute later", f.pass(), "Dev: New work from Ben: "+job.Number)
	f.do(func(tx pgx.Tx) error {
		var tries int
		err := tx.QueryRow(f.ctx, `SELECT tries FROM notices WHERE tenant_id = current_tenant() AND result = 'sent' AND item_id::text = $1
			AND person_id::text = $2 AND kind = 'new_work' ORDER BY id DESC LIMIT 1`, job.ID, f.people["dev"]).Scan(&tries)
		if err == nil && tries != 1 {
			t.Errorf("tries = %d, want 1", tries)
		}
		return err
	})
}

// TestPauseDoesNotHoldUpOthers: an admin presses Pause while a reader's pass holds the bookmark. Pause
// waits for the pass, but people's changes in the business don't wait for Pause.
func TestPauseDoesNotHoldUpOthers(t *testing.T) {
	f := newFirm(t)
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 2)
	go func() { // a long reader's pass
		done <- db.WithTenant(f.ctx, f.app, f.tenant.ID, func(tx pgx.Tx) error {
			if _, err := tx.Exec(f.ctx, `SELECT 1 FROM notice_bookmark WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	go func() {
		done <- db.WithTenant(f.ctx, f.app, f.tenant.ID, func(tx pgx.Tx) error {
			_, err := notify.Pause(f.ctx, tx, true, f.people["chitra"])
			return err
		})
	}()
	time.Sleep(200 * time.Millisecond) // Pause is now waiting for the bookmark
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	err := db.WithTenant(ctx, f.app, f.tenant.ID, func(tx pgx.Tx) error { return ledger.Lock(ctx, tx) })
	cancel()
	if err != nil {
		t.Errorf("a change waited for Pause: %v", err)
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	f.do(func(tx pgx.Tx) error {
		s, err := notify.LoadSettings(f.ctx, tx)
		if err == nil && !s.Paused() {
			t.Error("not paused")
		}
		return err
	})
}

// twoWaiting makes one waiting phone note each for Dev and Ben (a hand-over to each), read but not
// yet delivered.
func (f *firm) twoWaiting() {
	f.t.Helper()
	toDev, toBen := f.heldBy(1, "ben")[0], f.heldBy(1, "dev")[0]
	f.passOn(toDev.ID, "ben", "dev")
	f.passOn(toBen.ID, "dev", "ben")
	if _, err := f.r.Read(f.ctx, f.tenant); err != nil {
		f.t.Fatal(err)
	}
}

// TestLaterFailureKeepsMarks: the sender takes the first person's phone note, and then something
// breaks (here the sender itself, on the second). The first note stays marked sent: the next pass
// sends only the second, and nobody gets a note twice.
func TestLaterFailureKeepsMarks(t *testing.T) {
	f := newFirm(t)
	f.twoWaiting()
	before := f.results()
	f.out.whenSending(func(call int) {
		if call == 2 {
			panic("the fake sender breaks")
		}
	})
	f.r.Pass(f.ctx)
	if !strings.Contains(f.log.String(), "the fake sender breaks") {
		t.Errorf("the reader logged %q; want the failure", f.log.String())
	}
	f.log.Reset()
	first := f.told()
	if len(first) != 1 {
		t.Fatalf("before the failure, told %q; want one person", first)
	}
	if r := f.results(); r[notify.Sent] != before[notify.Sent]+1 || r[""] != 1 {
		t.Errorf("notices by result: %v (before the pass %v); want the first sent and the second waiting", r, before)
	}
	again := f.pass()
	if len(again) != 1 || again[0] == first[0] {
		t.Errorf("the next pass told %q; want only the other person (the first was told %q)", again, first)
	}
	want(t, "the pass after", f.pass())
}

// TestPauseStopsNotesOnTheirWay: an admin presses Pause while a phone note is being handed on. Pause
// waits until that note is handed on and marked, and then no other note goes, as the page says.
// After Resume, the one that waited goes.
func TestPauseStopsNotesOnTheirWay(t *testing.T) {
	f := newFirm(t)
	f.twoWaiting()
	sending, release := make(chan struct{}), make(chan struct{})
	f.out.whenSending(func(call int) {
		if call == 1 {
			close(sending)
			<-release // a slow push service
		}
	})
	delivered, paused := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := f.r.Deliver(f.ctx, f.tenant)
		delivered <- err
	}()
	select {
	case <-sending:
	case err := <-delivered:
		t.Fatalf("Deliver ended (%v) without handing a note on", err)
	}
	go func() {
		paused <- db.WithTenant(f.ctx, f.app, f.tenant.ID, func(tx pgx.Tx) error {
			_, err := notify.Pause(f.ctx, tx, true, f.people["chitra"])
			return err
		})
	}()
	waits := []chan error{delivered, paused}
	select {
	case err := <-paused:
		t.Errorf("Pause didn't wait for the note on its way (%v)", err)
		waits = waits[:1]
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	for _, c := range waits {
		if err := <-c; err != nil {
			t.Error(err)
		}
	}
	first := f.told()
	if len(first) != 1 {
		t.Fatalf("told %q; want only the note that was on its way when Pause was pressed", first)
	}
	want(t, "paused", f.pass())
	f.pause(false)
	again := f.pass()
	if len(again) != 1 || again[0] == first[0] {
		t.Errorf("after Resume, told %q; want only the other person (the first was told %q)", again, first)
	}
}
