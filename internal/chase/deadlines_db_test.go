//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/setup"
	"seyal/internal/testdb"
	"seyal/internal/thread"
	"seyal/internal/workitem"
)

// office is a fresh copy of the demo business (all made up) on the shipped samples, so job evaluation
// 0.6 with its timing: Asha in Sales, Ben, Dev and Priya in Operations, Chitra and Lakshmi (Owners and
// admins) and Elan (an Owner, not an admin), all with logins. Every day is a working day here, so a
// pretend day n days on is simply today + n.
type office struct {
	t      *testing.T
	ctx    context.Context
	owner  *pgxpool.Pool
	app    *pgxpool.Pool
	tenant db.Tenant
	people map[string]string // demo key → person id
	job    string            // job evaluation's version id
	zone   *time.Location
	today  time.Time // the real today, midnight in the business's time zone
	out    bytes.Buffer
}

func newOffice(t *testing.T) *office {
	t.Helper()
	return openOffice(t, false)
}

// openOffice is newOffice, with the demo's recurring Monthly return too when schedules is true (D75,
// R). Without it a pretend day past the 1st of a month would also start the month's returns, so the
// other tests leave it out.
func openOffice(t *testing.T, schedules bool) *office {
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
	if !schedules {
		f.Processes = slices.DeleteFunc(f.Processes, func(id string) bool { return id == "monthly-return" })
		if err := f.Check(lib); err != nil { // the processes it imports are worked out by Check
			t.Fatal(err)
		}
	}
	for i, p := range f.People {
		f.People[i].Email = p.Key + "@" + f.Business.Slug + ".kaveri-demo.example"
	}
	sum, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	o := &office{t: t, ctx: ctx, owner: owner, app: app, tenant: db.Tenant{ID: sum.TenantID, Slug: f.Business.Slug},
		people: sum.People}
	var ids []string
	for _, key := range []string{"asha", "ben", "dev", "priya", "chitra", "lakshmi", "elan"} {
		ids = append(ids, o.people[key])
	}
	testdb.GiveLogins(t, owner, sum.TenantID, ids...)
	o.do(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'job-evaluation'`).Scan(&o.job)
	})
	if o.zone, err = time.LoadLocation("Asia/Kolkata"); err != nil {
		t.Fatal(err)
	}
	o.today = midnight(time.Now().In(o.zone))
	return o
}

func (o *office) do(fn func(tx pgx.Tx) error) {
	o.t.Helper()
	if err := db.WithTenant(o.ctx, o.app, o.tenant.ID, fn); err != nil {
		o.t.Fatal(err)
	}
}

func (o *office) try(fn func(tx pgx.Tx) error) error {
	return db.WithTenant(o.ctx, o.app, o.tenant.ID, fn)
}

func (o *office) by(key string) ledger.Actor { return ledger.PersonActor(o.people[key]) }

// chaser is a chaser for this office only.
func (o *office) chaser() *Chaser {
	c := New(o.app, log.New(&o.out, "", 0))
	c.List = func(context.Context) ([]db.Tenant, error) { return []db.Tenant{o.tenant}, nil }
	return c
}

// chase runs one round as if it were n days after today, and returns how many things it did.
func (o *office) chase(n int) int {
	o.t.Helper()
	done, err := o.chaser().asIf(o.ctx, o.tenant, o.today.AddDate(0, 0, n).Format(time.DateOnly))
	if err != nil {
		o.t.Fatalf("chase on day %d: %v", n, err)
	}
	return done
}

// startJob starts a job as Asha for Sundaram Textiles' monthly books and sends it to Operations.
func (o *office) startJob(extra engine.Fields) workitem.Item {
	o.t.Helper()
	fields := engine.Fields{"client": "sundaram-textiles", "nature_of_work": "monthly-books", "reason": "Books for the year",
		"integrity_check": "pass"}
	for k, v := range extra {
		fields[k] = v
	}
	var it workitem.Item
	o.do(func(tx pgx.Tx) error {
		var err error
		if it, err = workitem.Start(o.ctx, tx, workitem.NewItem{PlaybookVersionID: o.job, Via: "chat", Fields: fields,
			By: o.by("asha")}); err != nil {
			return err
		}
		it, err = workitem.Act(o.ctx, tx, it.ID, "send_to_ops", "", o.by("asha"))
		return err
	})
	if it.State != "with_ops" || it.Team != "ops" {
		o.t.Fatalf("the job is %s with %q/%q, want with_ops with the Ops team", it.State, it.AssigneeID, it.Team)
	}
	return it
}

func (o *office) take(id, key string) workitem.Item {
	o.t.Helper()
	var it workitem.Item
	o.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Take(o.ctx, tx, id, "", "panel", o.by(key))
		return err
	})
	return it
}

func (o *office) get(id string) workitem.Item {
	o.t.Helper()
	var it workitem.Item
	o.do(func(tx pgx.Tx) error {
		var err error
		it, err = workitem.Get(o.ctx, tx, id)
		return err
	})
	return it
}

// lines is the item's thread as people read it: "Who: text".
func (o *office) lines(id string) []string {
	o.t.Helper()
	var out []string
	o.do(func(tx pgx.Tx) error {
		entries, err := thread.For(o.ctx, tx, id)
		for _, e := range entries {
			out = append(out, e.Who+": "+e.Text)
		}
		return err
	})
	return out
}

// has checks that the thread has the line exactly once.
func (o *office) has(id, line string) {
	o.t.Helper()
	n := 0
	for _, l := range o.lines(id) {
		if l == line {
			n++
		}
	}
	if n != 1 {
		o.t.Errorf("the thread has %q %d times, want once; thread:\n%s", line, n, strings.Join(o.lines(id), "\n"))
	}
}

// change is one key of the latest event of kind about the item.
func (o *office) change(id, kind, key string) string {
	o.t.Helper()
	var v string
	o.do(func(tx pgx.Tx) error {
		return tx.QueryRow(o.ctx, `SELECT coalesce(body::jsonb #>> ARRAY['payload', 'change', $3], '') FROM ledger
			WHERE subject_id::text = $1 AND kind = $2 ORDER BY seq DESC LIMIT 1`, id, kind, key).Scan(&v)
	})
	return v
}

// day names a day as the reminders do: "Fri 6 Nov".
func (o *office) day() string { return o.today.Format("Mon 2 Jan") }

// TestRemindThenEscalate follows the story (D75, M6) on job evaluation 0.6: Dev takes the job; on day 2
// two copies of the app chase at the same moment and Dev gets exactly one reminder; on day 4 the job
// goes to the Owner team, without a new version, with Dev as the holder Seyal took it from; then
// nothing more chases that stay. Elan (an Owner, not an admin) may not work it; Chitra (an admin) may.
// When the approval is refused and the job comes back, it goes to Dev, not to the Owner team.
func TestRemindThenEscalate(t *testing.T) {
	o := newOffice(t)
	job := o.startJob(nil)
	job = o.take(job.ID, "dev")
	if n := o.chase(1); n != 0 {
		t.Errorf("day 1 did %d things, want nothing yet", n)
	}

	var wg sync.WaitGroup
	var one, two int
	wg.Go(func() { one, _ = o.chaser().asIf(o.ctx, o.tenant, o.today.AddDate(0, 0, 2).Format(time.DateOnly)) })
	wg.Go(func() { two, _ = o.chaser().asIf(o.ctx, o.tenant, o.today.AddDate(0, 0, 2).Format(time.DateOnly)) })
	wg.Wait()
	if one+two != 1 {
		t.Errorf("two copies on day 2 did %d + %d things, want one reminder in all; log:\n%s", one, two, o.out.String())
	}
	reminder := "Seyal: Reminder for Dev: this has been in With ops for 2 working days (since " + o.day() + "). Still needed: Fee agreed."
	o.has(job.ID, reminder)
	if it := o.get(job.ID); it.Reminders != 1 || it.RemindedAt == "" || it.Version != job.Version {
		t.Errorf("after the reminder: %d reminders at %q, version %d (was %d)", it.Reminders, it.RemindedAt, it.Version, job.Version)
	}
	if got := o.change(job.ID, "item.posted", "to"); got != "person:"+o.people["dev"] {
		t.Errorf("the reminder is for %q, want Dev", got)
	}
	if !strings.Contains(o.out.String(), "chase "+o.tenant.Slug+": "+job.Number+": reminded holder") ||
		strings.Contains(o.out.String(), "Dev") {
		t.Errorf("log = %q; want the item number and no names", o.out.String())
	}
	if o.chase(2)+o.chase(3) != 0 {
		t.Error("days 2 (again) and 3 did something; want nothing until day 4")
	}

	if n := o.chase(4); n != 1 {
		t.Fatalf("day 4 did %d things, want the escalation; log:\n%s", n, o.out.String())
	}
	job = o.get(job.ID)
	if job.Team != "owner" || job.AssigneeID != "" || job.HandoffFrom != "" || !job.EscalatedTo() || job.Version != 1 {
		t.Errorf("escalated: with %q/%q from %q, escalated %v, version %d; want the Owner team, nobody handing it over, version 1",
			job.AssigneeID, job.Team, job.HandoffFrom, job.EscalatedTo(), job.Version)
	}
	o.has(job.ID, "Seyal: handed it to the Owner team (escalated): waited 4 working days in With ops (since "+o.day()+
		"), with Dev since "+o.day()+".")
	for key, want := range map[string]string{"how": "escalate", "from_holder": o.people["dev"], "from_assignee": "",
		"chase": "escalate", "escalate_to": "role:owner", "escalate_after": "4 days", "remind_after": "2 days",
		"today": o.today.AddDate(0, 0, 4).Format(time.DateOnly), "as_if": o.today.AddDate(0, 0, 4).Format(time.DateOnly)} {
		if got := o.change(job.ID, "item.assigned", key); got != want {
			t.Errorf("the escalation's %s = %q, want %q", key, got, want)
		}
	}
	if n := o.chase(6) + o.chase(10); n != 0 {
		t.Errorf("after the escalation, %d more things were done; want nothing in this stay", n)
	}

	// Elan is in the Owner team but not in Operations: he may pass it on, not work the step.
	err := o.try(func(tx pgx.Tx) error {
		_, err := workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("elan"))
		return err
	})
	if !errors.Is(err, workitem.ErrNotYours) {
		t.Errorf("Elan's submit = %v; want it refused", err)
	}
	if ok, _ := workitem.MayPass(job, workitem.Actor(o.people["elan"], []string{"owner"})); !ok {
		t.Error("Elan should be allowed to pass the escalated job on")
	}

	// Chitra, an admin, fills in the fee and submits it herself; Elan refuses the Owner approval.
	o.do(func(tx pgx.Tx) error {
		if _, err := workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1200000.0}, o.by("chitra")); err != nil {
			return err
		}
		var err error
		job, err = workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("chitra"))
		return err
	})
	if job.State != "pending_approval" {
		t.Fatalf("after Chitra's submit: %s", job.State)
	}
	o.do(func(tx pgx.Tx) error {
		var err error
		job, err = workitem.Decide(o.ctx, tx, job.ID, o.people["elan"], "owner", engine.Reject, "Check the fee with Ops")
		return err
	})
	if job.State != "draft" || job.AssigneeID != o.people["asha"] {
		t.Fatalf("after the refusal: %s with %q, want draft with Asha", job.State, job.AssigneeID)
	}
	o.do(func(tx pgx.Tx) error {
		var err error
		job, err = workitem.Act(o.ctx, tx, job.ID, "send_to_ops", "", o.by("asha"))
		return err
	})
	if job.State != "with_ops" || job.AssigneeID != o.people["dev"] || job.Reminders != 0 || job.EscalatedAt != "" {
		t.Errorf("sent to Ops again: %s with %q/%q, %d reminders, escalated %q; want Dev, in a new stay",
			job.State, job.AssigneeID, job.Team, job.Reminders, job.EscalatedAt)
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestApprovalReminders: in the approval step Seyal reminds the approvals still missing once, after 1
// working day, without changing the version, so approvals already given still count. A change to the details resets the approvals and starts
// the clock again, so whoever must approve again is reminded again.
func TestApprovalReminders(t *testing.T) {
	o := newOffice(t)
	job := o.startJob(engine.Fields{"fee_agreed": 1200000.0})
	job = o.take(job.ID, "dev")
	o.do(func(tx pgx.Tx) error {
		var err error
		job, err = workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("dev"))
		return err
	})
	if job.State != "pending_approval" {
		t.Fatalf("submitted: %s", job.State)
	}
	o.do(func(tx pgx.Tx) error { // Dev approves for Operations at once; the Owner approval is still missing
		_, err := workitem.Decide(o.ctx, tx, job.ID, o.people["dev"], "ops", engine.Approve, "")
		return err
	})
	if n := o.chase(1) + o.chase(1); n != 1 {
		t.Errorf("day 1 twice did %d things, want one reminder", n)
	}
	o.has(job.ID, "Seyal: Reminder: waiting 1 working day (since "+o.day()+") for approval from Owner.")
	if got := o.change(job.ID, "item.posted", "to"); got != "approvals:owner" {
		t.Errorf("the reminder is for %q, want the owner approval only", got)
	}
	// The reminder changed nothing that approvals depend on: Dev's approval still counts.
	o.do(func(tx pgx.Tx) error {
		it, err := workitem.Get(o.ctx, tx, job.ID)
		if err != nil {
			return err
		}
		st, err := workitem.Approvals(o.ctx, tx, job.ID)
		if it.Version != job.Version || it.Reminders != 1 || len(st.Waiting) != 1 || st.Waiting[0] != "owner" {
			t.Errorf("after the reminder: version %d (was %d), %d reminders, waiting for %v; want only the owner",
				it.Version, job.Version, it.Reminders, st.Waiting)
		}
		return err
	})
	if n := o.chase(3); n != 0 {
		t.Errorf("day 3 did %d things; want no second reminder in the same stay", n)
	}

	// Chitra changes the reason: a new version, so Dev's approval no longer counts, and the clock restarts.
	before := o.get(job.ID)
	o.do(func(tx pgx.Tx) error {
		var err error
		job, err = workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"reason": "Books for the year, from April"}, o.by("chitra"))
		return err
	})
	if job.State != "pending_approval" || job.Version == before.Version || job.StepSince <= before.StepSince || job.Reminders != 0 {
		t.Fatalf("after the edit: %s, version %d (was %d), stay %q (was %q), %d reminders; want a new stay",
			job.State, job.Version, before.Version, job.StepSince, before.StepSince, job.Reminders)
	}
	if n := o.chase(3); n != 1 {
		t.Errorf("day 3 after the edit did %d things, want a new reminder", n)
	}
	o.has(job.ID, "Seyal: Reminder: waiting 3 working days (since "+o.day()+") for approval from Operations and Owner.")
	clean(t, o.app, place{tenant: o.tenant})
}

// Made-up processes for where escalated work goes (all fictional).
const (
	toField = `id: chase-to-field
version: "1"
description: A made-up process that escalates to the person in a field.
numbering: "F-{seq:4}"
title: "{what}"
roles: [ops]
fields:
  - { key: what, label: What, type: text, required: true }
  - { key: notes, label: Notes, type: text, required: true }
  - { key: reviewer, label: Reviewer, type: person }
states: [open, done]
initial: open
steps:
  open: { owner: "role:ops", remind_after: 1 day, escalate_after: 2 days, escalate_to: "field:reviewer" }
transitions:
  - { action: finish, from: [open], to: done }
`
	toEmptyTeam = `id: chase-to-team
version: "1"
description: A made-up process that escalates to a team nobody is in.
numbering: "T-{seq:4}"
title: "{what}"
roles: [ops, audit]
fields:
  - { key: what, label: What, type: text, required: true }
states: [open, done]
initial: open
steps:
  open: { owner: "role:ops", remind_after: 1 day, escalate_after: 2 days, escalate_to: "role:audit" }
transitions:
  - { action: finish, from: [open], to: done }
`
	freeWork = `id: chase-free
version: "1"
description: A made-up process whose step has no owner, for work Seyal starts.
numbering: "W-{seq:4}"
title: "{what}"
roles: [ops]
fields:
  - { key: what, label: What, type: text, required: true }
states: [open, done]
initial: open
steps:
  open: { remind_after: 1 day }
transitions:
  - { action: finish, from: [open], to: done }
`
)

// startMade starts an item of a made-up process (registered once) with what and fields, as Asha, or
// as Seyal when by is the system.
func (o *office) startMade(source string, by ledger.Actor, fields engine.Fields) workitem.Item {
	o.t.Helper()
	var it workitem.Item
	o.do(func(tx pgx.Tx) error {
		p, err := playbook.Parse([]byte(source))
		if err == nil {
			err = p.Validate()
		}
		if err != nil {
			return err
		}
		pv, _, err := playbook.Register(o.ctx, tx, p)
		if err != nil {
			return err
		}
		it, err = workitem.Start(o.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", Fields: fields, By: by})
		return err
	})
	return it
}

// TestEscalateWhere: escalated work goes to the person in the step's field; if nobody is named, the
// person can't take work, or a team has nobody in it, nothing moves and the thread says so (once); if
// it is already with that person, the thread says it is still waiting. Work nobody has is reminded too.
func TestEscalateWhere(t *testing.T) {
	o := newOffice(t)
	asha, chitra, dev, karthik := o.by("asha"), o.people["chitra"], o.people["dev"], o.people["karthik"]
	toChitra := o.startMade(toField, asha, engine.Fields{"what": "Check the lease", "reviewer": chitra})
	nobody := o.startMade(toField, asha, engine.Fields{"what": "Check the insurance"})
	toDev := o.startMade(toField, asha, engine.Fields{"what": "Check the rent", "reviewer": dev})
	noLogin := o.startMade(toField, asha, engine.Fields{"what": "Check the meter", "reviewer": karthik})
	emptyTeam := o.startMade(toEmptyTeam, asha, engine.Fields{"what": "Count the stock"})
	free := o.startMade(freeWork, ledger.SystemActor, engine.Fields{"what": "Tidy the shelves"})
	for _, it := range []workitem.Item{toChitra, nobody, toDev, noLogin, emptyTeam} {
		o.take(it.ID, "dev")
	}

	if n := o.chase(1); n != 6 {
		t.Fatalf("day 1 did %d things, want 6 reminders; log:\n%s", n, o.out.String())
	}
	o.has(toChitra.ID, "Seyal: Reminder for Dev: this has been in Open for 1 working day (since "+o.day()+"). Still needed: Notes.")
	o.has(free.ID, "Seyal: Reminder: nobody has taken this; it has been in Open for 1 working day (since "+o.day()+"). Anyone can take it.")
	if n := o.chase(2); n != 5 {
		t.Fatalf("day 2 did %d things, want 5 escalations; log:\n%s", n, o.out.String())
	}
	since := o.day()
	if it := o.get(toChitra.ID); it.AssigneeID != chitra || !it.EscalatedTo() {
		t.Errorf("escalated to the reviewer: with %q, want Chitra", it.AssigneeID)
	}
	o.has(toChitra.ID, "Seyal: handed it to Chitra (escalated): waited 2 working days in Open (since "+since+"), with Dev since "+since+".")
	o.has(nobody.ID, "Seyal: Couldn't escalate: nobody is named as Reviewer. It stays with Dev.")
	o.has(toDev.ID, "Seyal: Still waiting after 2 working days in Open (since "+since+"). It is already with Dev.")
	o.has(noLogin.ID, "Seyal: Couldn't escalate: Karthik Subramanian has no login yet and is the Reviewer. It stays with Dev.")
	o.has(emptyTeam.ID, "Seyal: Couldn't escalate: nobody in audit can take work. It stays with Dev.")

	var members []workitem.Member
	o.do(func(tx pgx.Tx) error {
		var err error
		members, err = workitem.Members(o.ctx, tx)
		return err
	})
	for _, tc := range []struct {
		it    workitem.Item
		stuck bool
	}{{toChitra, false}, {nobody, true}, {toDev, false}, {noLogin, true}, {emptyTeam, true}} {
		it := o.get(tc.it.ID)
		var p *playbook.Playbook
		o.do(func(tx pgx.Tx) error {
			var err error
			p, err = workitem.PlaybookFor(o.ctx, tx, it)
			return err
		})
		if it.EscalatedAt == "" || workitem.CouldNotEscalate(p, it, members) != tc.stuck {
			t.Errorf("%s: escalated %q, couldn't escalate %v; want marked, and %v", it.Title, it.EscalatedAt,
				workitem.CouldNotEscalate(p, it, members), tc.stuck)
		}
	}
	if n := o.chase(5); n != 0 {
		t.Errorf("day 5 did %d things; each stay is escalated (or tried) once", n)
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestPausedBusiness: "chase: paused" in a business's set-up file stops its chasing, logged once;
// once it is taken out again, the work is chased as normal.
func TestPausedBusiness(t *testing.T) {
	o := newOffice(t)
	job := o.startJob(nil)
	o.take(job.ID, "dev")
	pause := func(on bool) {
		if _, err := o.owner.Exec(o.ctx, "UPDATE tenants SET chase_paused = $2 WHERE id::text = $1", o.tenant.ID, on); err != nil {
			t.Fatal(err)
		}
	}
	pause(true)
	c := o.chaser()
	for range 2 {
		if n, err := c.asIf(o.ctx, o.tenant, o.today.AddDate(0, 0, 3).Format(time.DateOnly)); n != 0 || err != nil {
			t.Errorf("a paused business: %d things, %v; want nothing", n, err)
		}
	}
	if strings.Count(o.out.String(), "paused in its set-up file") != 1 {
		t.Errorf("log = %q; want the pause said once", o.out.String())
	}
	pause(false)
	if n, err := c.asIf(o.ctx, o.tenant, o.today.AddDate(0, 0, 3).Format(time.DateOnly)); n != 1 || err != nil {
		t.Errorf("after the pause: %d things, %v; want the reminder", n, err)
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestStampNotBeforeHandOver: the chaser reads the time once per business, before its round. If the
// item was passed on after that, the reminder is stored at the hand-over, not before it, so the new
// holder counts as reminded: on the escalation day Seyal escalates, without a second reminder.
func TestStampNotBeforeHandOver(t *testing.T) {
	o := newOffice(t)
	job := o.take(o.startJob(nil).ID, "dev")
	c := workitem.Chase{StepSince: job.StepSince, Days: 2, Now: time.Now().Add(-time.Minute), Zone: o.zone}
	o.do(func(tx pgx.Tx) error {
		_, err := workitem.Remind(o.ctx, tx, job.ID, c)
		return err
	})
	job = o.get(job.ID)
	if job.Reminders != 1 || job.RemindedAt < job.HeldSince {
		t.Errorf("%d reminders at %q, Dev has it since %q; want one reminder, not before Dev got it",
			job.Reminders, job.RemindedAt, job.HeldSince)
	}
	var p *playbook.Playbook
	o.do(func(tx pgx.Tx) error {
		var err error
		p, err = workitem.PlaybookFor(o.ctx, tx, job)
		return err
	})
	if what, _ := due(p, job, Calendar{Zone: o.zone}, o.today.AddDate(0, 0, 4)); what != escalateDue {
		t.Errorf("on day 4: %q is due; want the escalation (Dev was reminded)", what)
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestFailedEscalationAfterHandOver: Dev took the items after the chaser read the time, and then the
// escalation moves nothing (nobody in the team can take work, or it is already with the reviewer). That
// must not read as "Seyal handed it on": the admins still see "Couldn't escalate", and who may act
// stays as it was.
func TestFailedEscalationAfterHandOver(t *testing.T) {
	o := newOffice(t)
	asha := o.by("asha")
	emptyTeam := o.take(o.startMade(toEmptyTeam, asha, engine.Fields{"what": "Count the stock"}).ID, "dev")
	toDev := o.take(o.startMade(toField, asha, engine.Fields{"what": "Check the rent", "reviewer": o.people["dev"]}).ID, "dev")
	var members []workitem.Member
	o.do(func(tx pgx.Tx) error {
		var err error
		members, err = workitem.Members(o.ctx, tx)
		return err
	})
	for _, tc := range []struct {
		it    workitem.Item
		stuck bool
	}{{emptyTeam, true}, {toDev, false}} {
		c := workitem.Chase{StepSince: tc.it.StepSince, Days: 2, Now: time.Now().Add(-time.Minute), Zone: o.zone}
		o.do(func(tx pgx.Tx) error {
			_, err := workitem.Escalate(o.ctx, tx, tc.it.ID, c)
			return err
		})
		it := o.get(tc.it.ID)
		var p *playbook.Playbook
		o.do(func(tx pgx.Tx) error {
			var err error
			p, err = workitem.PlaybookFor(o.ctx, tx, it)
			return err
		})
		if it.EscalatedAt <= it.HeldSince || it.EscalatedTo() || it.Engine().Escalated ||
			workitem.CouldNotEscalate(p, it, members) != tc.stuck || it.AssigneeID != o.people["dev"] {
			t.Errorf("%s: escalated at %q, Dev has it since %q, escalated to %v, couldn't escalate %v; want after the hand-over, "+
				"not handed on, couldn't escalate %v, still with Dev", it.Title, it.EscalatedAt, it.HeldSince, it.EscalatedTo(),
				workitem.CouldNotEscalate(p, it, members), tc.stuck)
		}
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestNobodyLeftToApprove: the job waits for the Owner approval only, then the work type is marked fixed
// price, so no approval is missing any more, but nothing moves the job. The chaser doesn't pick it up
// (it would find nobody to remind, mark nothing, and lock the ledger for it every round).
func TestNobodyLeftToApprove(t *testing.T) {
	o := newOffice(t)
	job := o.take(o.startJob(engine.Fields{"fee_agreed": 1200000.0}).ID, "dev")
	o.do(func(tx pgx.Tx) error {
		var err error
		if job, err = workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("dev")); err != nil {
			return err
		}
		_, err = workitem.Decide(o.ctx, tx, job.ID, o.people["dev"], "ops", engine.Approve, "")
		return err
	})
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	found := func() bool {
		var tasks []Task
		o.do(func(tx pgx.Tx) error {
			var err error
			tasks, err = findDeadlines(o.ctx, tx, Business{Tenant: o.tenant, Zone: o.zone, Calendar: Calendar{Zone: o.zone},
				Today: o.today.AddDate(0, 0, 1), Now: time.Now()})
			return err
		})
		return len(tasks) == 1 && tasks[0].Key == job.ID
	}
	if !found() {
		t.Fatal("before the change, the Owner approval is missing: want the job's reminder found on day 1")
	}
	o.do(func(tx pgx.Tx) error {
		wt, err := master.Get(o.ctx, tx, "work_type", "monthly-books")
		if err != nil {
			return err
		}
		wt.Fields["fixed_price"] = true
		_, _, err = master.Save(o.ctx, tx, lib.Masters["work_type"], wt.Key, wt.Name, wt.Fields, o.by("chitra"))
		return err
	})
	o.do(func(tx pgx.Tx) error {
		st, err := workitem.Approvals(o.ctx, tx, job.ID)
		if it, _ := workitem.Get(o.ctx, tx, job.ID); err == nil && (it.State != "pending_approval" || len(st.Waiting) != 0) {
			t.Fatalf("after the change: %s, waiting for %v; want pending_approval with nothing missing", it.State, st.Waiting)
		}
		return err
	})
	if found() {
		t.Error("with no approval missing, the job was still found to remind")
	}
	if n := o.chase(1); n != 0 {
		t.Errorf("day 1 did %d things, want nothing", n)
	}
	if it := o.get(job.ID); it.Reminders != 0 || it.State != "pending_approval" {
		t.Errorf("%d reminders, in %s; want none, still waiting", it.Reminders, it.State)
	}
	clean(t, o.app, place{tenant: o.tenant})
}
