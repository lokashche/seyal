//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
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
	"seyal/internal/outbox"
	"seyal/internal/playbook"
	"seyal/internal/send"
	"seyal/internal/setup"
	"seyal/internal/testdb"
	"seyal/internal/thread"
	"seyal/internal/workitem"
)

// fakeApp stands in for the demo's "Jobs sheet (demo)": it keeps every post and answers with code and
// reply. Nothing leaves this computer.
type fakeApp struct {
	mu     sync.Mutex
	posts  []post
	code   int
	reply  string
	onPost func() // if set, runs after each post (with no lock held): something happening mid-pass
}

type post struct {
	address string
	header  http.Header
	body    []byte
}

func (f *fakeApp) PostRedirect(_ context.Context, address string, _ []string, header http.Header, body []byte) (send.Answer, error) {
	f.mu.Lock()
	f.posts = append(f.posts, post{address, header, body})
	a, hook := send.Answer{Status: f.code, Body: []byte(f.reply)}, f.onPost
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return a, nil
}

func (f *fakeApp) answer(code int, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code, f.reply = code, reply
}

func (f *fakeApp) got() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

// firm is a fresh copy of the demo business (all made up) whose Job evaluation is the demo-only copy
// that sends approved jobs to "Jobs sheet (demo)" (examples/seed/job-evaluation-send.yaml), with a fake
// for that other app and a clock the test moves.
type firm struct {
	t      *testing.T
	ctx    context.Context
	owner  *pgxpool.Pool
	app    *pgxpool.Pool
	tenant db.Tenant
	people map[string]string
	job    string // the version id of Job evaluation
	fake   *fakeApp
	s      *outbox.Sender
	clock  time.Time
	log    bytes.Buffer
}

func newFirm(t *testing.T) *firm {
	t.Helper()
	return newFirmWith(t, nil)
}

// newFirmWith is newFirm with change made to the set-up file before it is imported.
func newFirmWith(t *testing.T, change func(f *setup.File)) *firm {
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
	f.Business.DaysOff = nil
	f.Processes = []string{"job-evaluation-send.yaml"} // the demo-only copy, next to demo.yaml
	f.OtherApps[0].Expect = send.ExpectOKJSON          // the way an Apps Script answers
	if change != nil {
		change(f)
	}
	if err := f.Check(lib); err != nil {
		t.Fatal(err)
	}
	for i, p := range f.People {
		f.People[i].Email = p.Key + "@" + f.Business.Slug + ".kaveri-demo.example"
	}
	sum, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	fm := &firm{t: t, ctx: ctx, owner: owner, app: app, tenant: db.Tenant{ID: sum.TenantID, Slug: f.Business.Slug},
		people: sum.People, fake: &fakeApp{code: http.StatusOK, reply: `{"ok":true,"row":12}`}, clock: time.Now()}
	var ids []string
	for _, key := range []string{"asha", "ben", "dev", "chitra", "elan", "karthik"} {
		ids = append(ids, fm.people[key])
	}
	testdb.GiveLogins(t, owner, sum.TenantID, ids...)
	fm.do(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'job-evaluation'`).Scan(&fm.job)
	})
	hosts, err := send.ParseHosts("sheet.kaveri-demo.example, hooks.kaveri-demo.example")
	if err != nil {
		t.Fatal(err)
	}
	fm.s = outbox.New(app, log.New(&fm.log, "", 0), true, hosts, fm.fake)
	fm.s.List = func(context.Context) ([]db.Tenant, error) { return []db.Tenant{fm.tenant}, nil }
	fm.s.Now = func() time.Time { return fm.clock }
	return fm
}

func (f *firm) do(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := db.WithTenant(f.ctx, f.app, f.tenant.ID, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *firm) by(key string) ledger.Actor { return ledger.PersonActor(f.people[key]) }

// toApproval starts a job as Asha for Sundaram Textiles' monthly books, lets Ben take it in Operations,
// agree ₹15,000 with ₹5,000 advance and submit it.
func (f *firm) toApproval() workitem.Item {
	f.t.Helper()
	return f.toApprovalFor("Books for the year")
}

// toApprovalFor is toApproval with reason as the job's reason.
func (f *firm) toApprovalFor(reason string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.do(func(tx pgx.Tx) error {
		var err error
		if it, err = workitem.Start(f.ctx, tx, workitem.NewItem{PlaybookVersionID: f.job, Via: "chat", By: f.by("asha"),
			Fields: engine.Fields{"client": "sundaram-textiles", "nature_of_work": "monthly-books", "reason": reason,
				"integrity_check": "pass"}}); err != nil {
			return err
		}
		if it, err = workitem.Act(f.ctx, tx, it.ID, "send_to_ops", "", f.by("asha")); err != nil {
			return err
		}
		if it, err = workitem.Take(f.ctx, tx, it.ID, "", "panel", f.by("ben")); err != nil {
			return err
		}
		if it, err = workitem.SetFields(f.ctx, tx, it.ID, engine.Fields{"fee_agreed": 1500000.0, "advance": 500000.0}, f.by("ben")); err != nil {
			return err
		}
		it, err = workitem.Act(f.ctx, tx, it.ID, "submit", "", f.by("ben"))
		return err
	})
	if it.State != "pending_approval" {
		f.t.Fatalf("the job is %s, want pending_approval", it.State)
	}
	return it
}

// approver is who gives each approval slot here.
var approver = map[string]string{"ops": "ben", "sales": "asha", "owner": "chitra"}

// approve gives every approval still needed, the Owner's (Chitra's) last.
func (f *firm) approve(id string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.do(func(tx pgx.Tx) error {
		st, err := workitem.Approvals(f.ctx, tx, id)
		if err != nil {
			return err
		}
		slices.SortFunc(st.Slots, func(a, b engine.SlotStatus) int { return strings.Compare(a.Slot, b.Slot) }) // owner, then ops, then sales
		slices.Reverse(st.Slots)
		for _, s := range st.Slots {
			if s.Required {
				if it, err = workitem.Decide(f.ctx, tx, id, f.people[approver[s.Slot]], s.Slot, engine.Approve, ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if it.State != "approved" {
		f.t.Fatalf("the job is %s after every approval, want approved", it.State)
	}
	if now := time.Now(); f.clock.Before(now) {
		f.clock = now // the send was queued at the database's own now
	}
	return it
}

// sends is the item's sends.
func (f *firm) sends(id string) []workitem.Send {
	f.t.Helper()
	var list []workitem.Send
	f.do(func(tx pgx.Tx) error {
		var err error
		list, err = workitem.ItemSends(f.ctx, tx, id)
		return err
	})
	return list
}

// one is the item's only send.
func (f *firm) one(id string) workitem.Send {
	f.t.Helper()
	list := f.sends(id)
	if len(list) != 1 {
		f.t.Fatalf("%d sends, want 1", len(list))
	}
	return list[0]
}

// pass runs one pass of the sender at the firm's clock.
func (f *firm) pass() int {
	f.t.Helper()
	return f.s.Pass(f.ctx)
}

// events is the item's ledger events of kind: each one's change and actor.
func (f *firm) events(id, kind string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	f.do(func(tx pgx.Tx) error {
		rows, err := tx.Query(f.ctx, `SELECT body::jsonb #> '{payload,change}' FROM ledger
			WHERE subject_id = $1::uuid AND kind = $2 ORDER BY seq`, id, kind)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[map[string]any])
		return err
	})
	return out
}

// lines is the item's thread as people read it: "Who: text".
func (f *firm) lines(id string) []string {
	f.t.Helper()
	var out []string
	f.do(func(tx pgx.Tx) error {
		entries, err := thread.For(f.ctx, tx, id)
		for _, e := range entries {
			out = append(out, e.Who+": "+e.Text)
		}
		return err
	})
	return out
}

func (f *firm) has(id, line string) {
	f.t.Helper()
	if !slices.Contains(f.lines(id), line) {
		f.t.Errorf("the thread has no %q; it has:\n  %s", line, strings.Join(f.lines(id), "\n  "))
	}
}

// clean checks `seyal verify`: the ledger chain, and every table (sends too) against the ledger.
func (f *firm) clean() {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		rep, err := ledger.Verify(f.ctx, tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			f.t.Errorf("ledger problems: %+v", rep.Problems)
		}
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) > 0 {
			f.t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
}

// noAddress checks that the sender's log never holds the app's address.
func (f *firm) noAddress() {
	f.t.Helper()
	if strings.Contains(f.log.String(), "AAAA0000") || strings.Contains(f.log.String(), "/macros/") {
		f.t.Errorf("the log holds the other app's address:\n%s", f.log.String())
	}
}

// TestApprovalSends: Chitra's approval queues the send in the same transaction (item.send_queued, with
// the exact body); the other app being down doesn't undo the approval, and the send waits and tries
// again; once the app answers OK, item.send_done follows and the thread says so in plain words. Every
// try has the same webhook-id and the same signed body, and verify stays clean.
func TestApprovalSends(t *testing.T) {
	f := newFirm(t)
	f.fake.answer(http.StatusServiceUnavailable, "")
	job := f.approve(f.toApproval().ID)

	queued := f.events(job.ID, workitem.KindSendQueued)
	if len(queued) != 1 || queued[0]["to"] != "jobs-sheet" || queued[0]["label"] != "Jobs sheet (demo)" || queued[0]["after"] != nil {
		t.Fatalf("send_queued events %v, want one to jobs-sheet, after a person's approval", queued)
	}
	var body struct {
		Type, ID, Business string
		Data               struct {
			Number, Process, State string
			Fields                 map[string]any
			Row                    []any
		}
	}
	if err := json.Unmarshal([]byte(queued[0]["body"].(string)), &body); err != nil {
		t.Fatal(err)
	}
	wantRow := []any{job.Number, "Sundaram Textiles", "Monthly bookkeeping", 15000.0, "INR", 5000.0, 10.0, "approved", body.ID}
	if body.Type != "job-evaluation.approved" || body.Business != f.tenant.Slug || body.Data.Number != job.Number ||
		!slices.Equal(body.Data.Row, wantRow) || !strings.HasPrefix(body.ID, "snd_"+f.tenant.Slug+"_") {
		t.Errorf("body %+v; want row %v", body, wantRow)
	}
	if fee, _ := body.Data.Fields["fee_agreed"].(map[string]any); fee["minor"] != 1500000.0 || fee["text"] != "₹15,000.00" {
		t.Errorf("fee_agreed went as %v", body.Data.Fields["fee_agreed"])
	}
	if s := f.one(job.ID); !s.Waiting() || s.Tries != 0 {
		t.Errorf("before any pass the send is %+v, want waiting", s)
	}

	// The app is down: the approval stands, and the send waits 5 minutes.
	if done := f.pass(); done != 0 || len(f.fake.got()) != 1 {
		t.Fatalf("a pass with the app down: %d done, %d posts", done, len(f.fake.got()))
	}
	if it := f.get(job.ID); it.State != "approved" || it.Version != job.Version {
		t.Errorf("after a failed send the job is %s v%d, want approved v%d", it.State, it.Version, job.Version)
	}
	s := f.one(job.ID)
	if !s.Waiting() || s.Tries != 1 || s.LastReason != "answered 503" || !s.NextAt.Equal(f.clock.UTC().Add(5*time.Minute).Truncate(time.Microsecond)) {
		t.Errorf("after one try: %+v, want waiting, 1 try, answered 503, next in 5 minutes", s)
	}
	f.clock = f.clock.Add(4 * time.Minute)
	if f.pass(); len(f.fake.got()) != 1 {
		t.Error("tried again before the 5 minutes were up")
	}

	// The app is back: done.
	f.fake.answer(http.StatusOK, `{"ok":true,"row":12}`)
	f.clock = f.clock.Add(time.Minute)
	if done := f.pass(); done != 1 {
		t.Fatalf("a pass with the app back: %d done, want 1; log:\n%s", done, f.log.String())
	}
	posts := f.fake.got()
	if len(posts) != 2 || posts[0].header.Get("webhook-id") != body.ID || posts[1].header.Get("webhook-id") != body.ID {
		t.Errorf("%d posts; each try must carry webhook-id %s", len(posts), body.ID)
	}
	var secret string
	if err := db.WithTenant(f.ctx, f.app, f.tenant.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT secret FROM other_apps WHERE name = 'jobs-sheet'`).Scan(&secret)
	}); err != nil {
		t.Fatal(err)
	}
	last := posts[1]
	at, _ := strconv.ParseInt(last.header.Get("webhook-timestamp"), 10, 64)
	if sig, err := send.Sign(secret, body.ID, time.Unix(at, 0), last.body); err != nil || sig != last.header.Get("webhook-signature") ||
		string(last.body) != queued[0]["body"] {
		t.Errorf("the post isn't the queued body signed with the app's secret (%v)", err)
	}
	if s := f.one(job.ID); !s.Done || s.Tries != 2 {
		t.Errorf("after the app took it: %+v", s)
	}
	if done := f.events(job.ID, workitem.KindSendDone); len(done) != 1 || done[0]["reason"] != "answered OK" || done[0]["tries"] != 2.0 {
		t.Errorf("send_done events %v", done)
	}
	f.has(job.ID, "Seyal: sent "+job.Number+" to “Jobs sheet (demo)” (it answered OK)")
	f.clock = f.clock.Add(time.Hour)
	if f.pass(); len(f.fake.got()) != 2 {
		t.Error("a send that was done went again")
	}
	if it := f.get(job.ID); it.Version != job.Version {
		t.Errorf("the send events changed the version: v%d, want v%d", it.Version, job.Version)
	}
	f.noAddress()
	f.clean()

	// A send's body changed in the table (by hand, as the owner) is found by verify.
	if err := db.WithTenant(f.ctx, f.owner, f.tenant.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `UPDATE sends SET body = replace(body, 'Sundaram', 'Somebody')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.do(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) != 1 || !strings.Contains(problems[0].String(), "its body was changed outside Seyal") {
			t.Errorf("verify after a body was changed: %v", problems)
		}
		return err
	})
}

func (f *firm) get(id string) workitem.Item {
	f.t.Helper()
	var it workitem.Item
	f.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Get(f.ctx, tx, id)
		return err
	})
	return it
}

// TestRetriesThenGivesUp: an app that keeps failing is tried after 5 minutes, 30 minutes, 2 hours and 6
// hours, then given up (item.send_failed); an admin's Send again queues the same send again, which
// then goes.
func TestRetriesThenGivesUp(t *testing.T) {
	f := newFirm(t)
	f.fake.answer(http.StatusInternalServerError, "")
	job := f.approve(f.toApproval().ID)
	for i, wait := range []time.Duration{0, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour} {
		f.clock = f.clock.Add(wait)
		f.pass()
		if n := len(f.fake.got()); n != i+1 {
			t.Fatalf("after try %d: %d posts", i+1, n)
		}
	}
	s := f.one(job.ID)
	if !s.Failed || s.Expired || s.Tries != 5 {
		t.Fatalf("after 5 tries: %+v, want given up", s)
	}
	failed := f.events(job.ID, workitem.KindSendFailed)
	if len(failed) != 1 || failed[0]["gave_up"] != true || failed[0]["reason"] != "answered 500" {
		t.Errorf("send_failed events %v", failed)
	}
	f.has(job.ID, "Seyal: gave up sending to “Jobs sheet (demo)” after 5 tries (answered 500). An admin can press Send again.")
	f.clock = f.clock.Add(24 * time.Hour)
	if f.pass(); len(f.fake.got()) != 5 {
		t.Error("a send that was given up went again by itself")
	}

	// Send again: only for a failed send, as the admin; the same id and body go.
	f.do(func(tx pgx.Tx) error {
		return workitem.SendAgain(f.ctx, tx, job.ID, s.ID, f.by("chitra"))
	})
	f.fake.answer(http.StatusOK, `{"ok":true}`)
	f.clock = time.Now() // Send again starts the send's 24 hours from the database's own now
	if done := f.pass(); done != 1 {
		t.Fatalf("after Send again: %d done", done)
	}
	posts := f.fake.got()
	if len(posts) != 6 || posts[5].header.Get("webhook-id") != s.ID {
		t.Errorf("Send again posted %d, id %q", len(posts)-5, posts[len(posts)-1].header.Get("webhook-id"))
	}
	again := f.events(job.ID, workitem.KindSendQueued)
	if len(again) != 2 || again[1]["again"] != true || again[1]["send"] != s.ID {
		t.Errorf("send_queued events %v, want the first and Send again", again)
	}
	f.has(job.ID, "Chitra: pressed Send again for “Jobs sheet (demo)”")
	f.do(func(tx pgx.Tx) error {
		if err := workitem.SendAgain(f.ctx, tx, job.ID, s.ID, f.by("chitra")); err != workitem.ErrNotFailed {
			t.Errorf("Send again for a send that went = %v, want ErrNotFailed", err)
		}
		return nil
	})
	f.noAddress()
	f.clean()
}

// TestOKFalseIsNotRetried: an Apps Script that ran but failed ("ok": false) is not tried again: an admin
// must fix it. Its reason is in plain words.
func TestOKFalseIsNotRetried(t *testing.T) {
	f := newFirm(t)
	f.fake.answer(http.StatusOK, `{"ok":false,"why":"no tab called Jobs"}`)
	job := f.approve(f.toApproval().ID)
	f.pass()
	f.clock = f.clock.Add(24 * time.Hour)
	f.pass()
	if n := len(f.fake.got()); n != 1 {
		t.Errorf(`"ok": false was posted %d times, want once`, n)
	}
	if s := f.one(job.ID); !s.Failed || s.LastReason != "script failed: no tab called Jobs" {
		t.Errorf("the send is %+v", s)
	}
	f.has(job.ID, "Seyal: couldn't send to “Jobs sheet (demo)”: script failed: no tab called Jobs. An admin must fix it, then press Send again.")
	f.clean()
}

// TestGonePausesTheApp: 410 (the address was switched off) gives the send up and pauses the app (by
// Seyal, a ledger event); the next approval's send waits while it is paused.
func TestGonePausesTheApp(t *testing.T) {
	f := newFirm(t)
	f.fake.answer(http.StatusGone, "")
	first := f.approve(f.toApproval().ID)
	f.pass()
	if s := f.one(first.ID); !s.Failed {
		t.Fatalf("after 410: %+v, want given up", s)
	}
	var why string
	f.do(func(tx pgx.Tx) error {
		app, err := send.Find(f.ctx, tx, "jobs-sheet")
		why = app.State.Why
		return err
	})
	if why != send.WhyGone {
		t.Errorf("the app's pause is %q, want gone", why)
	}
	f.has(first.ID, "Seyal: couldn't send to “Jobs sheet (demo)”: its address was switched off (answered 410), so Seyal paused it. An admin must fix it, then press Send again.")

	second := f.approve(f.toApproval().ID)
	f.clock = f.clock.Add(time.Hour)
	f.pass()
	if n := len(f.fake.got()); n != 1 {
		t.Errorf("%d posts while the app is paused, want only the first", n)
	}
	if s := f.one(second.ID); !s.Waiting() || s.LastReason != "paused" {
		t.Errorf("the second send while paused: %+v", s)
	}
	f.clean()

	// How a send ended, changed in the table by the app's own role (which may update those columns), is
	// found by verify: a failed send marked left with no Leave it, a waiting one marked done with nothing
	// sent.
	f.do(func(tx pgx.Tx) error {
		if _, err := tx.Exec(f.ctx, `UPDATE sends SET left_at = now() WHERE id = $1`, f.one(first.ID).ID); err != nil {
			return err
		}
		_, err := tx.Exec(f.ctx, `UPDATE sends SET done_at = now() WHERE id = $1`, f.one(second.ID).ID)
		return err
	})
	f.do(func(tx pgx.Tx) error {
		problems, err := audit.CheckTables(f.ctx, tx)
		if len(problems) != 2 {
			t.Errorf("verify after two sends' endings were changed: %v, want 2 problems", problems)
		}
		for _, p := range problems {
			if !strings.Contains(p.String(), "how it ended differs from the ledger") {
				t.Errorf("verify: %v", p)
			}
		}
		return err
	})
}

// TestPauseStopsClaimedSends: a Pause pressed while a pass is posting stops the sends that pass already
// claimed: each send reads its app again just before it goes. The send held back is let go untried, and
// waits while the app is paused.
func TestPauseStopsClaimedSends(t *testing.T) {
	f := newFirm(t)
	first := f.approve(f.toApproval().ID)
	second := f.approve(f.toApprovalFor("Books for next year").ID)
	f.fake.onPost = func() {
		f.fake.onPost = nil
		f.do(func(tx pgx.Tx) error {
			_, err := send.Pause(f.ctx, tx, "jobs-sheet", true, f.people["chitra"])
			return err
		})
	}
	f.pass()
	if n := len(f.fake.got()); n != 1 {
		t.Fatalf("%d posts after a Pause mid-pass, want only the one before it", n)
	}
	sent := 0
	for _, id := range []string{first.ID, second.ID} {
		s := f.one(id)
		switch {
		case s.Done:
			sent++
		case !s.Waiting() || s.Tries != 0 || s.LastReason != "paused":
			t.Errorf("the send held back: %+v, want waiting, untried, paused", s)
		}
	}
	if sent != 1 {
		t.Errorf("%d sends went, want 1", sent)
	}
	f.clock = f.clock.Add(time.Hour)
	f.pass()
	if n := len(f.fake.got()); n != 1 {
		t.Errorf("%d posts while paused, want still 1", n)
	}
	f.noAddress()
	f.clean()
}

// TestGoneStopsTheRestOfThePass: after a 410 pauses the app, the other sends to it that the same pass
// claimed don't go.
func TestGoneStopsTheRestOfThePass(t *testing.T) {
	f := newFirm(t)
	f.fake.answer(http.StatusGone, "")
	first := f.approve(f.toApproval().ID)
	second := f.approve(f.toApprovalFor("Books for next year").ID)
	f.pass()
	if n := len(f.fake.got()); n != 1 {
		t.Fatalf("%d posts after a 410, want 1", n)
	}
	failed, waiting := 0, 0
	for _, id := range []string{first.ID, second.ID} {
		switch s := f.one(id); {
		case s.Failed:
			failed++
		case s.Waiting() && s.Tries == 0 && s.LastReason == "paused":
			waiting++
		default:
			t.Errorf("send %+v", s)
		}
	}
	if failed != 1 || waiting != 1 {
		t.Errorf("%d failed and %d waiting, want 1 and 1", failed, waiting)
	}
	f.clean()
}

// TestWillSendShowsWhatGoes: the "will send" line shows the values as the send will freeze them: a copy
// listed before the send in the same step runs first. It says when the item's link goes too.
func TestWillSendShowsWhatGoes(t *testing.T) {
	old := workitem.PublicURL
	workitem.PublicURL = "https://seyal.example"
	t.Cleanup(func() { workitem.PublicURL = old })
	f := newFirmWith(t, func(sf *setup.File) { sf.OtherApps[0].Link = true })
	f.useVersion("0.7-send-copied", func(source string) string {
		source = strings.Replace(source, "    - do: send\n", "    - { do: copy, from: fee_agreed, to: advance }\n    - do: send\n", 1)
		return strings.Replace(source, "      link: false\n", "      link: true\n", 1)
	})
	it := f.toApproval()
	var plans []workitem.SendPlan
	f.do(func(tx pgx.Tx) error {
		var err error
		plans, err = workitem.SendPlans(f.ctx, tx, it)
		return err
	})
	want := it.Number + " · Sundaram Textiles · Monthly bookkeeping · ₹15,000.00 · ₹15,000.00 · 10"
	if len(plans) != 1 || plans[0].Line != want || !plans[0].Link {
		t.Fatalf("plans %+v, want one with %q and the link", plans, want)
	}
	f.approve(it.ID)
	queued := f.events(it.ID, workitem.KindSendQueued)
	if len(queued) != 1 {
		t.Fatalf("%d sends queued, want 1", len(queued))
	}
	var body struct {
		Data struct {
			Fields struct {
				Advance struct {
					Minor int64 `json:"minor"`
				} `json:"advance"`
			} `json:"fields"`
			Link string `json:"link"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(queued[0]["body"].(string)), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Fields.Advance.Minor != 1500000 || body.Data.Link == "" {
		t.Errorf("the body sent %+v, not what the line showed", body.Data)
	}
	f.clean()
}

// TestOldSendsExpire: with sending off on the server, a send waits, saying why, and after 24 hours it
// expires (item.send_failed, expired); switching sending on afterwards sends nothing old by itself.
func TestOldSendsExpire(t *testing.T) {
	f := newFirm(t)
	f.s.On = false
	job := f.approve(f.toApproval().ID)
	f.pass()
	if s := f.one(job.ID); !s.Waiting() || s.LastReason != "sending is switched off on this server" {
		t.Errorf("with sending off: %+v", s)
	}
	f.clock = f.clock.Add(25 * time.Hour)
	f.pass()
	if s := f.one(job.ID); !s.Failed || !s.Expired {
		t.Fatalf("after 25 hours: %+v, want expired", s)
	}
	f.has(job.ID, "Seyal: couldn't send to “Jobs sheet (demo)” within 24 hours (sending is switched off on this server). An admin can press Send again.")
	f.s.On = true
	f.pass()
	if n := len(f.fake.got()); n != 0 {
		t.Errorf("%d posts after sending was switched on, want none: old sends expire", n)
	}
	f.clean()
}

// TestSendBackQueuesNothing: work sent back into a step skips the step's actions (D68 choice 8), so a
// send there doesn't go again. Here a version sends from the draft step: starting queues one send, and
// the Owner's refusal (which sends the job back to the draft) queues none.
func TestSendBackQueuesNothing(t *testing.T) {
	f := newFirm(t)
	f.do(func(tx pgx.Tx) error {
		var source string
		if err := tx.QueryRow(f.ctx, `SELECT source FROM playbook_versions WHERE id = $1::uuid`, f.job).Scan(&source); err != nil {
			return err
		}
		source = strings.Replace(source, `version: "0.7-send"`, `version: "0.7-send-draft"`, 1)
		source = strings.Replace(source, "\non:\n", "\non:\n  draft:\n    - { do: send, to: jobs-sheet, fields: [client] }\n", 1)
		p, err := playbook.Parse([]byte(source))
		if err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		id, _, err := playbook.Register(f.ctx, tx, p)
		f.job = id
		return err
	})
	it := f.toApproval()
	if n := len(f.sends(it.ID)); n != 1 {
		t.Fatalf("starting in the draft step queued %d sends, want 1", n)
	}
	f.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Decide(f.ctx, tx, it.ID, f.people["chitra"], "owner", engine.Reject, "Fee too low")
		return err
	})
	if it.State != "draft" {
		t.Fatalf("after the refusal the job is %s, want draft", it.State)
	}
	if n := len(f.sends(it.ID)); n != 1 {
		t.Errorf("sent back to the draft step: %d sends, want still 1", n)
	}
	f.clean()
}

// useVersion registers a changed copy of the firm's Job evaluation as version, and starts new jobs on it.
func (f *firm) useVersion(version string, edit func(source string) string) {
	f.t.Helper()
	f.do(func(tx pgx.Tx) error {
		var source string
		if err := tx.QueryRow(f.ctx, `SELECT source FROM playbook_versions WHERE id = $1::uuid`, f.job).Scan(&source); err != nil {
			return err
		}
		changed := edit(strings.Replace(source, `version: "0.7-send"`, `version: "`+version+`"`, 1))
		p, err := playbook.Parse([]byte(changed))
		if err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		id, _, err := playbook.Register(f.ctx, tx, p)
		f.job = id
		return err
	})
}

// TestWaitingSendsDontBlockOthers: a pile of sends to a paused app (more than one pass looks at) doesn't
// keep a send to the business's other app from going: a send that waits is looked at again later, so
// the ones behind it come up.
func TestWaitingSendsDontBlockOthers(t *testing.T) {
	f := newFirmWith(t, func(sf *setup.File) {
		second := sf.OtherApps[0]
		second.Name, second.Label = "second-app", "Second app (demo)"
		second.URL = "https://hooks.kaveri-demo.example/webhook/0000"
		sf.OtherApps = append(sf.OtherApps, second)
	})
	job := f.approve(f.toApproval().ID)
	first := f.one(job.ID)
	f.do(func(tx pgx.Tx) error {
		if err := ledger.Lock(f.ctx, tx); err != nil {
			return err
		}
		_, err := send.SeyalPause(f.ctx, tx, "jobs-sheet", send.WhyCap)
		return err
	})
	// 120 older sends to the paused app, then one to the second app (made by hand, as the owner).
	if err := db.WithTenant(f.ctx, f.owner, f.tenant.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(f.ctx, `
			INSERT INTO sends (tenant_id, id, item_id, target, body, body_sha256, item_version, fields_sha256, queued_at, next_at)
			SELECT tenant_id, 'snd_' || $1 || '_' || (1000 + g), item_id, target, body, body_sha256, item_version, fields_sha256,
				now() - interval '1 hour', now() - interval '1 hour'
			FROM sends, generate_series(1, 120) g WHERE id = $2`, f.tenant.Slug, first.ID); err != nil {
			return err
		}
		_, err := tx.Exec(f.ctx, `
			INSERT INTO sends (tenant_id, id, item_id, target, body, body_sha256, item_version, fields_sha256, queued_at, next_at)
			SELECT tenant_id, 'snd_' || $1 || '_5000', item_id, 'second-app', body, body_sha256, item_version, fields_sha256,
				now() - interval '1 minute', now() - interval '1 minute'
			FROM sends WHERE id = $2`, f.tenant.Slug, first.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2 && len(f.fake.got()) == 0; i++ {
		f.pass()
	}
	posts := f.fake.got()
	if len(posts) != 1 || !strings.HasPrefix(posts[0].address, "https://hooks.kaveri-demo.example/") {
		t.Fatalf("%d posts after two passes; want the second app's send only", len(posts))
	}
	if id := posts[0].header.Get("webhook-id"); id != "snd_"+f.tenant.Slug+"_5000" {
		t.Errorf("posted %s, want the second app's send", id)
	}
}

// TestTooLongToSend: a field too long to send (a send's body may be at most 64 KB) doesn't undo the
// approval: the send ends at once, and the thread says why. Send again can't help; Leave it clears it.
func TestTooLongToSend(t *testing.T) {
	f := newFirmWith(t, func(sf *setup.File) {
		sf.OtherApps[0].Fields = append(sf.OtherApps[0].Fields, "reason")
	})
	f.useVersion("0.7-send-reason", func(source string) string {
		return strings.Replace(source, "fields: [client, nature_of_work, fee_agreed, advance, timeline_days]", "fields: [client, reason]", 1)
	})
	job := f.approve(f.toApprovalFor(strings.Repeat("Long scope. ", 6000)).ID) // about 72 KB
	s := f.one(job.ID)
	if !s.Failed || s.LastReason != workitem.ReasonTooBig || s.Tries != 0 {
		t.Fatalf("the send is %+v, want ended as too long", s)
	}
	f.has(job.ID, "Seyal: couldn't send to “Jobs sheet (demo)”: the item's details are too long to send (over 64 KB), so nothing went. An admin can press Leave it.")
	f.pass()
	if n := len(f.fake.got()); n != 0 {
		t.Errorf("%d posts of a send too long to go, want none", n)
	}
	f.do(func(tx pgx.Tx) error {
		if err := workitem.SendAgain(f.ctx, tx, job.ID, s.ID, f.by("chitra")); err != workitem.ErrCantGoAgain {
			t.Errorf("Send again for a send too long to go = %v, want ErrCantGoAgain", err)
		}
		return nil
	})
	f.do(func(tx pgx.Tx) error { return workitem.LeaveSend(f.ctx, tx, job.ID, s.ID, f.by("chitra")) })
	if s := f.one(job.ID); s.Failed || !s.Left || s.Waiting() {
		t.Errorf("after Leave it the send is %+v, want left", s)
	}
	f.has(job.ID, "Chitra: chose to leave the send to “Jobs sheet (demo)”: it won't go")
	f.do(func(tx pgx.Tx) error {
		if err := workitem.LeaveSend(f.ctx, tx, job.ID, s.ID, f.by("chitra")); err != workitem.ErrNotFailed {
			t.Errorf("Leave it twice = %v, want ErrNotFailed", err)
		}
		if err := workitem.SendAgain(f.ctx, tx, job.ID, s.ID, f.by("chitra")); err != workitem.ErrNotFailed {
			t.Errorf("Send again after Leave it = %v, want ErrNotFailed", err)
		}
		return nil
	})
	f.clean()
}

// TestRefusalIsAPersonsDecision: a send queued because an approver refused (the refusal moves the item
// back, not sent back to its holder) is not labelled "after a move by Seyal".
func TestRefusalIsAPersonsDecision(t *testing.T) {
	f := newFirm(t)
	f.useVersion("0.7-send-refused", func(source string) string {
		source = strings.Replace(source, "on_reject_back: true", "on_reject_back: false", 1)
		return strings.Replace(source, "\non:\n", "\non:\n  draft:\n    - { do: send, to: jobs-sheet, fields: [client] }\n", 1)
	})
	it := f.toApproval()
	f.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Decide(f.ctx, tx, it.ID, f.people["chitra"], "owner", engine.Reject, "Fee too low")
		return err
	})
	if it.State != "draft" {
		t.Fatalf("after the refusal the job is %s, want draft", it.State)
	}
	queued := f.events(it.ID, workitem.KindSendQueued)
	if len(queued) != 2 {
		t.Fatalf("%d sends queued, want 2 (the start, and the refusal)", len(queued))
	}
	for _, q := range queued {
		if q["after"] != nil {
			t.Errorf("a send queued after a person's start or refusal says after %v", q["after"])
		}
	}
	f.clean()
}
