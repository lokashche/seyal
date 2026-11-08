//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"context"
	"log"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/outbox"
	"seyal/internal/playbook"
	"seyal/internal/send"
	"seyal/internal/workitem"
)

// okApp stands in for the demo's other app: it answers 200 to every post. Nothing leaves this computer.
type okApp struct{ posts int }

func (a *okApp) PostRedirect(context.Context, string, []string, http.Header, []byte) (send.Answer, error) {
	a.posts++
	return send.Answer{Status: http.StatusOK}, nil
}

// TestSendLastKeepsWorkedEscalated: Chitra works an escalated step, then gives the last approval, which
// sends the job to the demo's other app (D96). The send's events come last in the item's ledger; each
// carries the unchanged snapshot, so "Chitra worked With ops after it was escalated" still holds, the
// thread shows both lines, and verify stays clean (design 2.3, "What is recorded"; D75 decision 11).
func TestSendLastKeepsWorkedEscalated(t *testing.T) {
	o := newOffice(t)
	o.do(func(tx pgx.Tx) error { // the demo-only copy of Job evaluation that sends approved jobs
		p, err := playbook.LoadFile("../../examples/seed/job-evaluation-send.yaml")
		if err != nil {
			return err
		}
		id, _, err := playbook.Register(o.ctx, tx, p)
		if err != nil {
			return err
		}
		o.job = id
		return playbook.Use(o.ctx, tx, id, o.by("chitra"))
	})
	job := o.startJob(nil)
	o.take(job.ID, "dev")
	if n := o.chase(2) + o.chase(4); n != 2 {
		t.Fatalf("days 2 and 4 did %d things, want a reminder and the escalation; log:\n%s", n, o.out.String())
	}
	o.take(job.ID, "chitra")
	o.do(func(tx pgx.Tx) error {
		if _, err := workitem.SetFields(o.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1200000.0}, o.by("chitra")); err != nil {
			return err
		}
		if _, err := workitem.Act(o.ctx, tx, job.ID, "submit", "", o.by("chitra")); err != nil {
			return err
		}
		for _, d := range []struct{ key, slot string }{{"dev", "ops"}, {"asha", "sales"}, {"chitra", "owner"}} {
			if _, err := workitem.Decide(o.ctx, tx, job.ID, o.people[d.key], d.slot, engine.Approve, ""); err != nil {
				return err
			}
		}
		return nil
	})
	if got := o.get(job.ID); got.State != "approved" {
		t.Fatalf("the job is %s, want approved", got.State)
	}

	app := &okApp{}
	hosts, _ := send.ParseHosts("sheet.kaveri-demo.example")
	s := outbox.New(o.app, log.New(&o.out, "", 0), true, hosts, app)
	s.List = func(context.Context) ([]db.Tenant, error) { return []db.Tenant{o.tenant}, nil }
	if done := s.Pass(o.ctx); done != 1 || app.posts != 1 {
		t.Fatalf("the sender: %d done, %d posts; want 1 and 1; log:\n%s", done, app.posts, o.out.String())
	}
	var last string
	o.do(func(tx pgx.Tx) error {
		return tx.QueryRow(o.ctx, `SELECT kind FROM ledger WHERE subject_id = $1::uuid ORDER BY seq DESC LIMIT 1`, job.ID).Scan(&last)
	})
	if last != workitem.KindSendDone {
		t.Fatalf("the item's last event is %s, want %s", last, workitem.KindSendDone)
	}
	o.do(func(tx pgx.Tx) error {
		steps, err := workitem.WorkedEscalated(o.ctx, tx, job.ID, o.people["chitra"])
		if err == nil && !slices.Equal(steps, []string{"with_ops"}) {
			t.Errorf("with a send last, Chitra worked %v; want With ops", steps)
		}
		return err
	})
	o.has(job.ID, "Chitra: approved as owner. Worked With ops after it was escalated")
	o.has(job.ID, "Seyal: sent "+job.Number+" to “Jobs sheet (demo)” (it answered OK)")
	clean(t, o.app, place{tenant: o.tenant})
}
