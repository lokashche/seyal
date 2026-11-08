// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/send"
	"seyal/internal/workitem"
)

// Other apps in "Leaves Seyal" (D95, design 2.3 and 2.4): one line per other app the business's
// set-up file lists, with [Send a test] and [Pause] / [Resume]. The address and the signing secret
// are never shown: only the host, and the address's end when it is a fixed word like "…/exec".

// testsPerMinute is how many tests one business may send a minute.
const testsPerMinute = 5

// testTimeout is the most a test may take, inside the request's own time.
const testTimeout = 30 * time.Second

// appLine is one other app in "Leaves Seyal".
type appLine struct {
	Name, Label, Service, Host, Ends string
	Fields                           string // the fields it may ever receive, "a, b"
	FilePaused                       bool   // paused: true in the set-up file
	Paused                           bool   // paused in the app (an admin or Seyal)
	PausedWhy                        string // "by Chitra at 11:02", or why Seyal paused it
	HostsOff                         string // its hosts not in SEYAL_SEND_HOSTS (its own, or one it redirects to)
	LastTest                         string // "on 8 Nov at 14:02: answered OK"
	// Its sends (D96): the last one that went ("at 14:02: answered OK"), how many wait, and the items
	// whose send failed or expired (an admin presses Send again on the item).
	LastSent string
	Waiting  int
	Failed   []failedSend
}

// failedSend is an item whose send to an other app failed or expired.
type failedSend struct{ ID, Number string }

// testView is what a test just sent: the exact data (the body before signing) and the answer.
type testView struct {
	Label, Line, Body, Answer string
	OK                        bool
}

// appLines reads the other apps for "Leaves Seyal". inUse are the business's processes: a field an
// app may receive is shown by its label there (its key when no process has it).
func (s *Server) appLines(ctx context.Context, tx pgx.Tx, c *call, inUse []playbook.InUse) ([]appLine, error) {
	targets, err := send.Targets(ctx, tx)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{} // field key → label
	for _, u := range inUse {
		if u.Playbook == nil {
			continue
		}
		for _, f := range u.Playbook.Fields {
			if _, ok := labels[f.Key]; !ok {
				labels[f.Key] = ask.Label(f)
			}
		}
	}
	names := map[string]string{} // person id → name
	name := func(id string) (string, error) {
		if id == "" {
			return "", nil
		}
		if n, ok := names[id]; ok {
			return n, nil
		}
		var n string
		err := tx.QueryRow(ctx, `SELECT display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`, id).Scan(&n)
		names[id] = n
		return n, err
	}
	var lines []appLine
	for _, t := range targets {
		fields := make([]string, len(t.Fields))
		for i, k := range t.Fields {
			if fields[i] = labels[k]; fields[i] == "" {
				fields[i] = k
			}
		}
		l := appLine{Name: t.Name, Label: t.Label, Service: t.Service, Host: t.Host(), Ends: t.AddressEnd(),
			Fields: strings.Join(fields, ", "), FilePaused: t.Paused, Paused: t.State.Paused(),
			HostsOff: strings.Join(t.HostsOff(s.sendHosts), ", ")}
		switch t.State.Why {
		case "admin":
			who, err := name(t.State.PausedBy)
			if err != nil {
				return nil, err
			}
			l.PausedWhy = "by " + who + " " + when(t.State.PausedAt, c.tenant.Zone)
		case "gone":
			l.PausedWhy = "by Seyal: the address was switched off (410)"
		case "cap":
			l.PausedWhy = "by Seyal: more than 30 sends in an hour"
		}
		if !t.State.TestedAt.IsZero() {
			who, err := name(t.State.TestedBy)
			if err != nil {
				return nil, err
			}
			l.LastTest = when(t.State.TestedAt, c.tenant.Zone) + " by " + who + ": " + t.State.TestResult
		}
		if err := sendCounts(ctx, tx, c, &l); err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// when shows a moment as "at 11:02" today, or "on 5 Nov at 11:02".
func when(t time.Time, zone *time.Location) string {
	at := t.In(zone)
	if at.Format(time.DateOnly) == time.Now().In(zone).Format(time.DateOnly) {
		return at.Format("at 15:04")
	}
	return at.Format("on 2 Jan at 15:04")
}

// otherApp is an admin's [Send a test], [Pause] or [Resume] for one other app.
func (s *Server) otherApp(w http.ResponseWriter, r *http.Request, c *call) {
	name := r.PathValue("name")
	switch do := r.PostFormValue("do"); do {
	case "pause", "resume":
		err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
			_, err := send.Pause(r.Context(), tx, name, do == "pause", c.session.PersonID)
			return err
		})
		if errors.Is(err, send.ErrNoTarget) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		notice := "Paused. Seyal sends nothing to this other app until you press Resume."
		if do == "resume" {
			notice = "Resumed. What waited while it was paused may still go, if it isn't too old."
		}
		s.showProcesses(w, r, c, "", http.StatusOK, notice, "")
	case "test":
		s.testOtherApp(w, r, c, name)
	default:
		s.showProcesses(w, r, c, "", http.StatusUnprocessableEntity, "", "Please press Send a test, Pause or Resume.")
	}
}

// testOtherApp sends fictional data to one other app, then shows exactly what went and the answer.
// The call is made outside any transaction; the result is kept afterwards in a short one of its own.
func (s *Server) testOtherApp(w http.ResponseWriter, r *http.Request, c *call, name string) {
	var t send.Listed
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		var err error
		t, err = send.Find(r.Context(), tx, name)
		return err
	})
	if errors.Is(err, send.ErrNoTarget) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	refuse := func(problem string) {
		s.showProcesses(w, r, c, "", http.StatusConflict, "", problem)
	}
	switch {
	case !s.sendOn:
		refuse("Sending to other apps is switched off on this Seyal server (SEYAL_SEND), so nothing was sent.")
		return
	case len(t.HostsOff(s.sendHosts)) > 0:
		refuse("This Seyal server may not send to " + strings.Join(t.HostsOff(s.sendHosts), ", ") +
			" (not in SEYAL_SEND_HOSTS), so nothing was sent.")
		return
	case t.Paused:
		refuse(t.Label + " is paused in the business's set-up file, so nothing was sent.")
		return
	case t.State.Paused():
		refuse(t.Label + " is paused. Press Resume first, then Send a test.")
		return
	case !s.testRate.take(c.tenant.ID):
		refuse("Too many tests in a minute. Please wait a minute and try again.")
		return
	}
	at := time.Now()
	id := send.TestID(c.tenant.Slug, at)
	body, err := send.TestBody(t.Target, c.tenant.Slug, c.tenant.Currency, id, at)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	v := send.Test(ctx, s.sender, t.Target, id, at, body)
	err = db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		return send.RecordTest(r.Context(), tx, name, t.Hash(), c.session.PersonID, v.Reason)
	})
	if errors.Is(err, send.ErrChanged) {
		// An import changed or removed this other app while the test was on its way: the answer is about
		// the old set-up, so it isn't kept or shown as the last test.
		s.showProcesses(w, r, c, "", http.StatusConflict, "",
			"The set-up of "+t.Label+" changed while the test was on its way, so its answer was not kept. Please send a test again.")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tv := &testView{Label: t.Label, Body: pretty(body), Answer: v.Reason, OK: v.Kind == send.Done,
		Line: send.TestNumber + " · " + send.TestName + " · " + workitem.Money(0, c.tenant.Currency)}
	// The result box is at the end of a long page, so the banner at the top says it too.
	notice := "Test sent to " + t.Label + ": " + v.Reason + ". What went is below, under Leaves Seyal."
	problem := ""
	if !tv.OK {
		notice, problem = "", "Test sent to "+t.Label+": "+v.Reason+". What went is below, under Leaves Seyal."
	}
	s.showProcessesWith(w, r, c, "", http.StatusOK, notice, problem, nil, tv)
}

// pretty lays JSON out on several lines, for people to read.
func pretty(body []byte) string {
	var b bytes.Buffer
	if json.Indent(&b, body, "", "  ") != nil {
		return string(body)
	}
	return b.String()
}

// sendAgain is an admin's [Send again] on an item, for a send that failed or expired (D96): the same
// send, with the same body and id, goes again from the start (tries and its 24 hours). It is an
// item.send_queued ledger event by the admin.
func (s *Server) sendAgain(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		return workitem.SendAgain(r.Context(), tx, id, r.PostFormValue("send"), ledger.PersonActor(c.session.PersonID))
	})
	switch {
	case errors.Is(err, workitem.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, workitem.ErrNotFailed):
		s.showScreen(w, r, c, id, http.StatusConflict, "This send hasn't failed, so there is nothing to send again.")
	case errors.Is(err, workitem.ErrCantGoAgain):
		s.showScreen(w, r, c, id, http.StatusConflict, "Sending this again can't work: it is too long to send, or the "+
			"business's set-up file no longer lists its other app. Press Leave it if it shouldn't go.")
	case err != nil:
		s.fail(w, r, err)
	default:
		notice := "Queued again. Seyal sends it in a moment."
		if !s.sendOn {
			notice = "Queued again, but sending is switched off on this server (SEYAL_SEND), so it waits."
		}
		s.show(w, r, c, id, http.StatusOK, "", notice)
	}
}

// leaveSend is an admin's [Leave it] on an item, for a send that failed or expired (D96): the send won't
// go, and it no longer counts as failed. It is an item.send_left ledger event by the admin.
func (s *Server) leaveSend(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		return workitem.LeaveSend(r.Context(), tx, id, r.PostFormValue("send"), ledger.PersonActor(c.session.PersonID))
	})
	switch {
	case errors.Is(err, workitem.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, workitem.ErrNotFailed):
		s.showScreen(w, r, c, id, http.StatusConflict, "This send hasn't failed, so there is nothing to leave.")
	case err != nil:
		s.fail(w, r, err)
	default:
		s.show(w, r, c, id, http.StatusOK, "", "Left. This send won't go.")
	}
}

// goneApps lists failed sends whose other app the set-up file no longer lists (D96), newest first (at
// most 20), so Leaves Seyal shows every failed send My Work counts.
func goneApps(ctx context.Context, tx pgx.Tx) ([]failedSend, error) {
	rows, err := tx.Query(ctx, `
		SELECT w.id::text, w.number FROM sends s JOIN work_items w ON w.tenant_id = s.tenant_id AND w.id = s.item_id
		WHERE s.tenant_id = current_tenant() AND (s.gave_up_at IS NOT NULL OR s.expired_at IS NOT NULL) AND s.left_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM other_apps a WHERE a.tenant_id = s.tenant_id AND a.name = s.target)
		ORDER BY coalesce(s.gave_up_at, s.expired_at) DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[failedSend])
}

// sendCounts fills an other app's line with its sends (D96): the last that went, how many wait, and
// the items whose send failed or expired, newest first (at most 20).
func sendCounts(ctx context.Context, tx pgx.Tx, c *call, l *appLine) error {
	var lastAt *time.Time
	var lastReason string
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE done_at IS NULL AND gave_up_at IS NULL AND expired_at IS NULL),
			max(done_at), coalesce((SELECT last_reason FROM sends WHERE tenant_id = current_tenant() AND target = $1
				AND done_at IS NOT NULL ORDER BY done_at DESC LIMIT 1), '')
		FROM sends WHERE tenant_id = current_tenant() AND target = $1`, l.Name).Scan(&l.Waiting, &lastAt, &lastReason); err != nil {
		return err
	}
	if lastAt != nil {
		l.LastSent = when(*lastAt, c.tenant.Zone) + ": " + lastReason
	}
	rows, err := tx.Query(ctx, `
		SELECT w.id::text, w.number FROM sends s JOIN work_items w ON w.tenant_id = s.tenant_id AND w.id = s.item_id
		WHERE s.tenant_id = current_tenant() AND s.target = $1 AND (s.gave_up_at IS NOT NULL OR s.expired_at IS NOT NULL)
			AND s.left_at IS NULL
		ORDER BY coalesce(s.gave_up_at, s.expired_at) DESC LIMIT 20`, l.Name)
	if err != nil {
		return err
	}
	l.Failed, err = pgx.CollectRows(rows, pgx.RowToStructByPos[failedSend])
	return err
}
