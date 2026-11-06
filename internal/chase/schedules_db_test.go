//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/master"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/start"
	"seyal/internal/workitem"
)

// Recurring work (D75, R), in a fresh demo with its sample Monthly return: Sundaram Textiles (Handled
// by Dev) and Annapoorna Foods (Handled by Ben) have "Files a monthly return" ticked. The schedule came
// into use when the demo was loaded (today), so its first run is the first 1st of a month from today.

// scheduleOffice is a demo office with the Monthly return, and its first run.
type scheduleOffice struct {
	*office
	first start.Period // the first run: on or after today
	next  start.Period // the run after it
	s     playbook.Schedule
}

func newScheduleOffice(t *testing.T) *scheduleOffice {
	t.Helper()
	o := &scheduleOffice{office: openOffice(t, true)}
	sample, err := playbook.LoadFile("../../examples/playbooks/monthly-return.yaml")
	if err != nil {
		t.Fatal(err)
	}
	o.s = *sample.Schedule
	o.first = start.Due(o.s, o.today, o.zone, 4)
	if o.first.RunDay.Format(time.DateOnly) < o.today.Format(time.DateOnly) {
		o.first = start.Next(o.s, o.first, 4) // today's run is for a period before the schedule was in use
	}
	o.next = start.Next(o.s, o.first, 4)
	return o
}

// on runs one round of c as if it were the given date (a run day, plus days), and returns what it did.
func (o *scheduleOffice) on(c *Chaser, d time.Time, plus int) int {
	o.t.Helper()
	day := d.AddDate(0, 0, plus).Format(time.DateOnly)
	done, err := c.asIf(o.ctx, o.tenant, day)
	if err != nil {
		o.t.Fatalf("chase as if %s: %v", day, err)
	}
	return done
}

// returns lists the business's scheduled items, by started_ref.
func (o *scheduleOffice) returns() map[string]workitem.Item {
	o.t.Helper()
	out := map[string]workitem.Item{}
	o.do(func(tx pgx.Tx) error {
		all, err := workitem.All(o.ctx, tx)
		for _, it := range all {
			if it.StartedVia == "schedule" {
				out[it.StartedRef] = it
			}
		}
		return err
	})
	return out
}

// runs counts the schedule.ran events, and those of them that held a run back.
func (o *scheduleOffice) runs() (all, held int) {
	o.t.Helper()
	o.do(func(tx pgx.Tx) error {
		return tx.QueryRow(o.ctx, `SELECT count(*), count(*) FILTER (WHERE body::jsonb #>> '{payload,held_back}' = 'true')
			FROM ledger WHERE kind = 'schedule.ran' AND actor_kind = 'system'`).Scan(&all, &held)
	})
	return all, held
}

func (o *scheduleOffice) ref(p start.Period, entry string) string {
	return "monthly-return:" + p.Key + ":" + entry
}

// useVersion registers a changed copy of the sample (a new version) and makes new work start on it.
func (o *scheduleOffice) useVersion(version, from, to string) string {
	o.t.Helper()
	src, err := playbook.LoadFile("../../examples/playbooks/monthly-return.yaml")
	if err != nil {
		o.t.Fatal(err)
	}
	text := strings.Replace(string(src.Source), `version: "0.1"`, `version: "`+version+`"`, 1)
	text = strings.Replace(text, from, to, 1)
	var id string
	o.do(func(tx pgx.Tx) error {
		p, err := playbook.Parse([]byte(text))
		if err != nil {
			return err
		}
		if id, _, err = playbook.Register(o.ctx, tx, p); err != nil {
			return err
		}
		return playbook.Use(o.ctx, tx, id, o.by("chitra"))
	})
	return id
}

// TestScheduleRuns: the first run is the first run day on or after the day the schedule came into use;
// it starts one item per ticked entry, for the person who handles it, due from the period, with the
// thread line and the ledger's reasons; running again, or two copies at once, starts nothing more; an
// entry ticked after the run waits for the next one; a cancelled item isn't started again; a new
// version starts nothing again, and the next run starts on it.
func TestScheduleRuns(t *testing.T) {
	o := newScheduleOffice(t)
	c := o.chaser()

	// The real today: nothing from a period before the schedule came into use.
	o.on(c, o.today, 0)
	if o.first.RunDay.Format(time.DateOnly) != o.today.Format(time.DateOnly) {
		if got := o.returns(); len(got) != 0 {
			t.Fatalf("a run before the schedule came into use started %d items", len(got))
		}
		if all, _ := o.runs(); all != 0 {
			t.Fatalf("%d schedule.ran events before the first run day", all)
		}
	}

	// The first run day: one item for each ticked entry.
	o.on(c, o.first.RunDay, 0)
	got := o.returns()
	if len(got) != 2 {
		t.Fatalf("the first run started %d items, want 2: %v", len(got), got)
	}
	st, ap := got[o.ref(o.first, "sundaram-textiles")], got[o.ref(o.first, "annapoorna-foods")]
	if st.Title != "Sundaram Textiles — "+o.first.Name || st.AssigneeID != o.people["dev"] || st.State != "prepare" ||
		st.DueOn != o.first.DueOn || st.Fields["period"] != o.first.Name || st.Fields["preparer"] != o.people["dev"] {
		t.Errorf("Sundaram's item: %+v", st)
	}
	if ap.AssigneeID != o.people["ben"] || ap.Title != "Annapoorna Foods — "+o.first.Name {
		t.Errorf("Annapoorna's item: %+v", ap)
	}
	o.has(st.ID, "Seyal: started this from the monthly schedule: "+o.first.Name+", for Sundaram Textiles.")
	if o.change(st.ID, "item.started", "via") != "schedule" || o.change(st.ID, "item.started", "period") != o.first.Name ||
		o.change(st.ID, "item.started", "reason") != "Monthly schedule: "+o.first.Name+", for Sundaram Textiles" ||
		o.change(st.ID, "item.started", "as_if") != o.first.RunDay.Format(time.DateOnly) {
		t.Errorf("item.started doesn't say why: via %q, period %q, reason %q, as_if %q", o.change(st.ID, "item.started", "via"),
			o.change(st.ID, "item.started", "period"), o.change(st.ID, "item.started", "reason"), o.change(st.ID, "item.started", "as_if"))
	}
	var actor, entries string
	o.do(func(tx pgx.Tx) error {
		return tx.QueryRow(o.ctx, `SELECT actor_kind, body::jsonb #>> '{payload,entries}' FROM ledger WHERE kind = 'schedule.ran'`).
			Scan(&actor, &entries)
	})
	if actor != "system" || entries != `["annapoorna-foods", "sundaram-textiles"]` {
		t.Errorf("schedule.ran by %s covers %s", actor, entries)
	}

	// Running again, and two copies of the app at the same moment: nothing more. (The pretend day is
	// weeks after the real one, so these rounds also remind Dev and Ben: Prepare reminds after 5 days.)
	o.on(c, o.first.RunDay, 0)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := o.chaser().asIf(o.ctx, o.tenant, o.first.RunDay.AddDate(0, 0, 1).Format(time.DateOnly)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n, all := len(o.returns()), 0; n != 2 {
		t.Errorf("after more rounds there are %d items, want 2", n)
	} else if all, _ = o.runs(); all != 1 {
		t.Errorf("%d schedule.ran events, want 1", all)
	}

	// Deccan Motors is ticked after the run: it waits for the next run.
	o.do(func(tx pgx.Tx) error {
		mk, err := playbook.Kind(o.ctx, tx, "client")
		if err != nil {
			return err
		}
		d, err := master.Get(o.ctx, tx, "client", "deccan-motors")
		if err != nil {
			return err
		}
		d.Fields["monthly_return"] = true
		_, _, err = master.Save(o.ctx, tx, mk, d.Key, d.Name, d.Fields, o.by("chitra"))
		return err
	})
	o.on(c, o.first.RunDay, 9)
	if n := len(o.returns()); n != 2 {
		t.Errorf("an entry ticked after the run got this period's item: %d items", n)
	}

	// Chitra cancels Sundaram's return, and a new version (with the same runs) is put in use: still
	// nothing more for this period.
	o.do(func(tx pgx.Tx) error {
		_, err := workitem.Act(o.ctx, tx, st.ID, "cancel", "Filed by the client themselves", o.by("chitra"))
		return err
	})
	v2 := o.useVersion("0.2", "A return filed every month", "A return filed each month")
	o.on(c, o.first.RunDay, 10)
	if n, all := len(o.returns()), 0; n != 2 {
		t.Errorf("after a cancel and a new version there are %d items, want 2", n)
	} else if all, _ = o.runs(); all != 1 {
		t.Errorf("%d schedule.ran events after the new version, want 1", all)
	}

	// The next run: Deccan Motors too, on version 0.2. Nobody handles it, so it is free work.
	o.on(c, o.next.RunDay, 0)
	got = o.returns()
	if len(got) != 5 {
		t.Fatalf("after the next run there are %d items, want 5", len(got))
	}
	dm := got[o.ref(o.next, "deccan-motors")]
	if dm.PlaybookVersionID != v2 || dm.AssigneeID != "" || dm.Team != "" || dm.DueOn != o.next.DueOn {
		t.Errorf("Deccan Motors' item: %+v", dm)
	}
	if got[o.ref(o.next, "sundaram-textiles")].ID == "" {
		t.Error("Sundaram Textiles got no item for the next period after the last one was cancelled")
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestScheduleHeldBack: more matches than max_per_run start none and write one schedule.ran with
// held_back (once, however many rounds); a new version with a higher limit then starts the run.
func TestScheduleHeldBack(t *testing.T) {
	o := newScheduleOffice(t)
	c := o.chaser()
	o.useVersion("0.2", "max_per_run: 30", "max_per_run: 1")
	y, m, d := o.first.RunDay.Date()
	// Over the limit, the round has only the decision to do, not one task per entry that would do
	// nothing (after a big list import, thousands of them, each taking the business's lock).
	o.do(func(tx pgx.Tx) error {
		tasks, err := findSchedules(o.ctx, tx, Business{Today: time.Date(y, m, d, 0, 0, 0, 0, o.zone)})
		if err == nil && len(tasks) != 1 {
			t.Errorf("over the limit, the round has %d schedule tasks, want 1 (the decision)", len(tasks))
		}
		return err
	})
	if n := o.on(c, o.first.RunDay, 0); n != 1 {
		t.Errorf("held back: %d things done, want 1 (the event)", n)
	}
	if n := o.on(c, o.first.RunDay, 1); n != 0 {
		t.Errorf("held back again: %d things done, want 0", n)
	}
	all, held := o.runs()
	if len(o.returns()) != 0 || all != 1 || held != 1 {
		t.Fatalf("held back: %d items, %d events (%d held back); want 0, 1, 1", len(o.returns()), all, held)
	}
	if !strings.Contains(o.out.String(), "held back: 2 match, the limit is 1") {
		t.Errorf("the log doesn't say it was held back:\n%s", o.out.String())
	}
	var p start.Plan
	o.do(func(tx pgx.Tx) error {
		var err error
		p, _, err = start.For(o.ctx, tx, "monthly-return", o.first.RunDay.AddDate(0, 0, 1))
		return err
	})
	if !p.HeldBack() || len(p.Entries) != 2 || p.Schedule.Limit() != 1 {
		t.Errorf("plan: held back %v, %d entries, limit %d", p.HeldBack(), len(p.Entries), p.Schedule.Limit())
	}

	o.useVersion("0.3", "max_per_run: 30", "max_per_run: 5")
	o.on(c, o.first.RunDay, 2)
	if all, held := o.runs(); len(o.returns()) != 2 || all != 2 || held != 1 {
		t.Errorf("after raising the limit: %d items, %d events (%d held back); want 2, 2, 1", len(o.returns()), all, held)
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestScheduleSwitchedOff: a process switched off for new work starts nothing; switched on again (by
// Switch on, or by choosing a version while it is off), its schedule counts from that moment (so a run whose day passed while it was off isn't caught up: see
// TestCameIntoUse in the start package for the day rule).
func TestScheduleSwitchedOff(t *testing.T) {
	o := newScheduleOffice(t)
	c := o.chaser()
	o.do(func(tx pgx.Tx) error { return playbook.SetActive(o.ctx, tx, "monthly-return", false, o.by("chitra")) })
	o.on(c, o.first.RunDay, 0)
	if n := len(o.returns()); n != 0 {
		t.Fatalf("a switched-off process started %d items", n)
	}
	var before time.Time
	o.do(func(tx pgx.Tx) error { return tx.QueryRow(o.ctx, "SELECT now()").Scan(&before) })
	o.do(func(tx pgx.Tx) error { return playbook.SetActive(o.ctx, tx, "monthly-return", true, o.by("chitra")) })
	var p start.Plan
	o.do(func(tx pgx.Tx) error {
		var err error
		p, _, err = start.For(o.ctx, tx, "monthly-return", o.first.RunDay)
		return err
	})
	if p.Since.Before(before) || !p.Active {
		t.Errorf("switched on again: in use since %v, want from %v on", p.Since, before)
	}

	// Switched off, then a new version with the same runs chosen: that switches it on again too, so it
	// also counts from that moment (playbook.Use writes only playbook.used).
	o.do(func(tx pgx.Tx) error { return playbook.SetActive(o.ctx, tx, "monthly-return", false, o.by("chitra")) })
	o.do(func(tx pgx.Tx) error { return tx.QueryRow(o.ctx, "SELECT now()").Scan(&before) })
	o.useVersion("0.2", "A return filed every month", "A return filed each month")
	o.do(func(tx pgx.Tx) error {
		var err error
		p, _, err = start.For(o.ctx, tx, "monthly-return", o.first.RunDay)
		return err
	})
	if p.Since.Before(before) || !p.Active || p.Playbook.Version != "0.2" {
		t.Errorf("a version chosen while off: in use since %v (want from %v on), on %v, version %s",
			p.Since, before, p.Active, p.Playbook.Version)
	}
	clean(t, o.app, place{tenant: o.tenant})
}

// TestScheduleStopped: a list change that removes the yes/no detail is refused while the schedule
// uses it; if the list lacks it anyway, the round starts nothing and logs it once a day.
func TestScheduleStopped(t *testing.T) {
	o := newScheduleOffice(t)
	c := o.chaser()
	var src string
	o.do(func(tx pgx.Tx) error {
		return tx.QueryRow(o.ctx, `SELECT source FROM master_kinds WHERE kind = 'client'`).Scan(&src)
	})
	without := strings.Replace(src, "  - { key: monthly_return, label: Files a monthly return, type: bool }\n", "", 1)
	if without == src {
		t.Fatal("the client list has no monthly_return line to take out")
	}
	err := o.try(func(tx pgx.Tx) error {
		mk, err := playbook.ParseMasterKind([]byte(without))
		if err != nil {
			return err
		}
		_, err = playbook.SaveKind(o.ctx, tx, mk, o.by("chitra"))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), `no yes/no detail "monthly_return"`) {
		t.Fatalf("taking the detail out: %v", err)
	}
	// A list without it all the same (for example from before this check): written straight into the
	// table, which `seyal verify` would report.
	o.do(func(tx pgx.Tx) error {
		_, err := tx.Exec(o.ctx, `UPDATE master_kinds SET source = $1 WHERE tenant_id = current_tenant() AND kind = 'client'`, without)
		return err
	})
	o.on(c, o.first.RunDay, 0)
	o.on(c, o.first.RunDay, 0)
	if n := len(o.returns()); n != 0 {
		t.Errorf("a stopped schedule started %d items", n)
	}
	if n := strings.Count(o.out.String(), `schedule stopped: the Client list has no yes/no detail "monthly_return"`); n != 1 {
		t.Errorf("the log says the schedule stopped %d times, want once:\n%s", n, o.out.String())
	}
}

// TestScheduleHolderCantTakeWork: when the entry's "Handled by" person can't take work, "Prepared by"
// stays empty and the item is free work that Ops can take; a late run keeps the period's due date.
func TestScheduleHolderCantTakeWork(t *testing.T) {
	o := newScheduleOffice(t)
	o.do(func(tx pgx.Tx) error {
		_, err := people.SetActive(o.ctx, tx, o.people["ben"], false, o.by("chitra"))
		return err
	})
	late := 21 // a run 21 days after its day is still this period's (the next one is a month later)
	o.on(o.chaser(), o.first.RunDay, late)
	ap := o.returns()[o.ref(o.first, "annapoorna-foods")]
	if ap.ID == "" || ap.AssigneeID != "" || ap.Team != "" || ap.Fields["preparer"] != nil {
		t.Fatalf("Annapoorna's item: %+v", ap)
	}
	if ap.DueOn != o.first.DueOn || !(ap.DueOn < o.first.RunDay.AddDate(0, 0, late).Format(time.DateOnly)) {
		t.Errorf("a late run is due %s, want %s (already past)", ap.DueOn, o.first.DueOn)
	}
	o.has(ap.ID, "Seyal: Ben is no longer active, so Prepared by is left empty. Anyone who may work this step can take it.")
	var members []workitem.Member
	var p *playbook.Playbook
	o.do(func(tx pgx.Tx) error {
		var err error
		if members, err = workitem.Members(o.ctx, tx); err != nil {
			return err
		}
		p, err = workitem.PlaybookFor(o.ctx, tx, ap)
		return err
	})
	dev := engine.Actor{Person: engine.Person{ID: o.people["dev"], Roles: []string{"ops"}}}
	if !workitem.MayTake(p, ap, dev, members) {
		t.Error("Dev (Operations) may not take the free work")
	}
	taken := o.take(ap.ID, "dev")
	if taken.AssigneeID != o.people["dev"] || taken.Fields["preparer"] != o.people["dev"] {
		t.Errorf("after Dev took it: %+v", taken)
	}
	clean(t, o.app, place{tenant: o.tenant})
}
