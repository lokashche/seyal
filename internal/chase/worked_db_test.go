//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/people"
	"seyal/internal/workitem"
)

// TestWorkedAfterEscalation: when someone does an escalated step's work themselves (Chitra, an admin,
// after Seyal handed the job to the Owner team) and then gives the Owner approval, the approval's ledger
// event and the thread say "Chitra worked With ops after it was escalated" (D75, decision 11). Taking
// it and passing it back doesn't count, and nor does work done before the escalation.
func TestWorkedAfterEscalation(t *testing.T) {
	// escalated starts a job, lets Dev take it, and lets Seyal escalate it to the Owner team.
	escalated := func(o *office) workitem.Item {
		t.Helper()
		job := o.startJob(nil)
		o.take(job.ID, "dev")
		if n := o.chase(2) + o.chase(4); n != 2 {
			t.Fatalf("days 2 and 4 did %d things, want a reminder and the escalation; log:\n%s", n, o.out.String())
		}
		job = o.get(job.ID)
		if !job.EscalatedTo() || job.Team != "owner" {
			t.Fatalf("the job is with %q/%q, want the Owner team after the escalation", job.AssigneeID, job.Team)
		}
		return job
	}
	decide := func(o *office, id, key, slot string) {
		t.Helper()
		o.do(func(tx pgx.Tx) error {
			_, err := workitem.Decide(o.ctx, tx, id, o.people[key], slot, engine.Approve, "")
			return err
		})
	}
	worked := func(o *office, id, key string) []string {
		t.Helper()
		var steps []string
		o.do(func(tx pgx.Tx) error {
			var err error
			steps, err = workitem.WorkedEscalated(o.ctx, tx, id, o.people[key])
			return err
		})
		return steps
	}

	t.Run("Chitra works the escalated step, then approves as Owner", func(t *testing.T) {
		o := newOffice(t)
		job := escalated(o)
		o.take(job.ID, "chitra")
		o.do(func(tx pgx.Tx) error {
			if _, err := workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1200000.0}, o.by("chitra")); err != nil {
				return err
			}
			_, err := workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("chitra"))
			return err
		})
		decide(o, job.ID, "dev", "ops") // Dev worked the step too, but before it was escalated
		decide(o, job.ID, "chitra", "owner")

		if got := o.change(job.ID, "item.decided", "worked_escalated"); got != `["with_ops"]` {
			t.Errorf("the Owner approval's worked_escalated = %q, want [\"with_ops\"]", got)
		}
		if steps := worked(o, job.ID, "chitra"); !slices.Equal(steps, []string{"with_ops"}) ||
			workitem.WorkedEscalatedLine("Chitra", steps) != "Chitra worked With ops after it was escalated" {
			t.Errorf("Chitra worked %v (%q); want With ops", steps, workitem.WorkedEscalatedLine("Chitra", steps))
		}
		if steps := worked(o, job.ID, "dev"); len(steps) != 0 {
			t.Errorf("Dev worked %v after the escalation, want nothing: his work came before it", steps)
		}
		o.has(job.ID, "Chitra: approved as owner. Worked With ops after it was escalated")
		for _, line := range o.lines(job.ID) {
			if strings.HasPrefix(line, "Dev: approved as ops") && strings.Contains(line, "escalated") {
				t.Errorf("Dev's approval shouldn't say he worked it after the escalation: %q", line)
			}
		}
		clean(t, o.app, place{tenant: o.tenant})
	})

	// Roles are judged when the work was done, not when she approves: giving herself Operations just
	// before approving (and taking it away after) doesn't hide the mark, and the card shows what the
	// approval's event recorded.
	t.Run("Chitra gives herself Operations after the work, before approving", func(t *testing.T) {
		o := newOffice(t)
		job := escalated(o)
		o.take(job.ID, "chitra")
		o.do(func(tx pgx.Tx) error {
			if _, err := workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1200000.0}, o.by("chitra")); err != nil {
				return err
			}
			if _, err := workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("chitra")); err != nil {
				return err
			}
			_, err := people.Grant(o.ctx, tx, o.people["chitra"], "ops", o.by("chitra"))
			return err
		})
		decide(o, job.ID, "dev", "ops")
		decide(o, job.ID, "chitra", "owner")
		if got := o.change(job.ID, "item.decided", "worked_escalated"); got != `["with_ops"]` {
			t.Errorf("the Owner approval's worked_escalated = %q, want [\"with_ops\"]: she wasn't in Operations when she worked it", got)
		}
		o.do(func(tx pgx.Tx) error {
			_, err := people.Revoke(o.ctx, tx, o.people["chitra"], "ops", o.by("chitra"))
			return err
		})
		if steps := worked(o, job.ID, "chitra"); !slices.Equal(steps, []string{"with_ops"}) {
			t.Errorf("after the role came and went Chitra worked %v; want With ops still", steps)
		}
		o.do(func(tx pgx.Tx) error {
			recorded, err := workitem.WorkedOnApprovals(o.ctx, tx, job.ID)
			if err == nil && (!slices.Equal(recorded["owner "+o.people["chitra"]], []string{"with_ops"}) ||
				len(recorded["ops "+o.people["dev"]]) != 0) {
				t.Errorf("the approvals' recorded escalated work = %v; want With ops on Chitra's Owner approval only", recorded)
			}
			return err
		})
		clean(t, o.app, place{tenant: o.tenant})
	})

	t.Run("Chitra is in Operations while she works it, and leaves it before approving", func(t *testing.T) {
		o := newOffice(t)
		job := escalated(o)
		o.do(func(tx pgx.Tx) error {
			_, err := people.Grant(o.ctx, tx, o.people["chitra"], "ops", o.by("chitra"))
			return err
		})
		o.take(job.ID, "chitra")
		o.do(func(tx pgx.Tx) error {
			if _, err := workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1200000.0}, o.by("chitra")); err != nil {
				return err
			}
			if _, err := workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("chitra")); err != nil {
				return err
			}
			_, err := people.Revoke(o.ctx, tx, o.people["chitra"], "ops", o.by("chitra"))
			return err
		})
		decide(o, job.ID, "dev", "ops")
		decide(o, job.ID, "chitra", "owner")
		if got := o.change(job.ID, "item.decided", "worked_escalated"); got != "" {
			t.Errorf("the Owner approval's worked_escalated = %q, want nothing: she was in Operations when she worked it", got)
		}
		clean(t, o.app, place{tenant: o.tenant})
	})

	t.Run("Chitra only passes it back to Dev, who does the work", func(t *testing.T) {
		o := newOffice(t)
		job := escalated(o)
		o.take(job.ID, "chitra")
		o.do(func(tx pgx.Tx) error {
			if _, _, err := workitem.Pass(o.ctx, tx, job.ID, workitem.PassOn{Person: o.people["dev"], Note: "Please finish by Monday"},
				o.by("chitra")); err != nil {
				return err
			}
			if _, err := workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1200000.0}, o.by("dev")); err != nil {
				return err
			}
			_, err := workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("dev"))
			return err
		})
		decide(o, job.ID, "chitra", "owner")
		if got := o.change(job.ID, "item.decided", "worked_escalated"); got != "" {
			t.Errorf("the Owner approval's worked_escalated = %q, want nothing: Chitra only passed it on", got)
		}
		if steps := worked(o, job.ID, "dev"); len(steps) != 0 {
			t.Errorf("Dev worked %v after the escalation; want nothing: he is in Operations, so it is his own step", steps)
		}
		clean(t, o.app, place{tenant: o.tenant})
	})
}
