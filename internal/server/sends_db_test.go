//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/outbox"
	"seyal/internal/send"
	"seyal/internal/workitem"
)

// TestSendsOnTheItem: the admin switches Job evaluation to the demo-only copy that sends approved jobs
// (Settings → Processes, as the "Try it" does); a version sending a field the app may not receive is
// refused. Before approval the sidebar shows exactly what will go; a failed send shows there with Send
// again for admins only, in the admin's briefing and in Leaves Seyal; Send again queues it, and it goes.
func TestSendsOnTheItem(t *testing.T) {
	ctx := context.Background()
	fake := &fakeApp{code: http.StatusBadRequest}
	s := newSiteWith(t, "../../examples/playbooks").withSend(t, true, demoHost, fake)
	admin, sales := s.logIn(lakshmi), s.logIn(arjun)
	settings := "/t/" + s.slug + "/settings/processes/job-evaluation"

	source, err := os.ReadFile("../../examples/seed/job-evaluation-send.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(strings.Replace(string(source), `version: "0.7-send"`, `version: "0.7-send-bad"`, 1),
		"fields: [client, nature_of_work, fee_agreed, advance, timeline_days]", "fields: [client, reason]", 1)
	if code, body, _ := s.post(admin, settings, url.Values{"action": {"save"}, "source": {bad}}, nil); code != http.StatusUnprocessableEntity ||
		!strings.Contains(html.UnescapeString(body), "jobs-sheet may not receive reason") {
		t.Errorf("a version sending a field the app may not receive: %d %s", code, problem(body))
	}
	if code, body, _ := s.post(admin, settings, url.Values{"action": {"save"}, "source": {string(source)}}, nil); code != http.StatusOK ||
		!strings.Contains(body, "Version 0.7-send saved") {
		t.Fatalf("saving the demo-only copy: %d %s", code, problem(body))
	}
	// "Use this version" with a version that isn't there (a stale form) says so in plain words.
	if _, body, _ := s.post(admin, settings, url.Values{"action": {"use"}, "version": {"00000000-0000-0000-0000-000000000000"}}, nil); !strings.Contains(html.UnescapeString(body), "That version doesn't exist.") ||
		strings.Contains(body, "no rows") {
		t.Errorf("using a version that isn't there: %s", problem(body))
	}
	if _, body, _ := s.get(admin, settings); !strings.Contains(html.UnescapeString(body),
		"send Client, Nature of work, Fee agreed, Advance, Timeline (days) to the other app jobs-sheet") {
		t.Error("the process's details don't say what it sends")
	}

	// A job waits for approval: the sidebar shows what will go, before anyone approves.
	by := func(key string) ledger.Actor { return ledger.PersonActor(s.people[key]) }
	var job workitem.Item
	in := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, s.app, s.tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	in(func(tx pgx.Tx) error {
		var version string
		if err := tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'job-evaluation'`).Scan(&version); err != nil {
			return err
		}
		var err error
		if job, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: version, Via: "chat", By: by(arjun),
			Fields: engine.Fields{"client": "sundaram-textiles", "nature_of_work": "monthly-books", "reason": "Books", "integrity_check": "pass"}}); err != nil {
			return err
		}
		if job, err = workitem.Act(ctx, tx, job.ID, "send_to_ops", "", by(arjun)); err != nil {
			return err
		}
		if job, err = workitem.Take(ctx, tx, job.ID, "", "panel", by(priya)); err != nil {
			return err
		}
		if job, err = workitem.SetFields(ctx, tx, job.ID, engine.Fields{"fee_agreed": 1500000.0, "advance": 500000.0}, by(priya)); err != nil {
			return err
		}
		job, err = workitem.Act(ctx, tx, job.ID, "submit", "", by(priya))
		return err
	})
	item := "/t/" + s.slug + "/items/" + job.ID
	page := func(c *http.Client, path string) string {
		t.Helper()
		code, body, _ := s.get(c, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		return html.UnescapeString(body)
	}
	body := page(admin, item)
	for _, want := range []string{"When approved, Seyal will send to “Jobs sheet (demo)”:",
		job.Number + " · Sundaram Textiles · Monthly bookkeeping · ₹15,000.00 · ₹5,000.00 · 10",
		"Leaves Seyal: these values, to Google (demo)."} {
		if !strings.Contains(body, want) {
			t.Errorf("before approval the sidebar has no %q", want)
		}
	}
	in(func(tx pgx.Tx) error {
		for _, d := range []struct{ key, slot string }{{priya, "ops"}, {arjun, "sales"}, {lakshmi, "owner"}} {
			var err error
			if job, err = workitem.Decide(ctx, tx, job.ID, s.people[d.key], d.slot, engine.Approve, ""); err != nil {
				return err
			}
		}
		return nil
	})
	if job.State != "approved" {
		t.Fatalf("the job is %s, want approved", job.State)
	}
	if strings.Contains(page(admin, item), "Seyal will send") {
		t.Error("an approved job still says what it will send")
	}

	// The app answers 400: the send fails; admins see Send again, others don't.
	hosts, _ := send.ParseHosts(demoHost)
	sender := outbox.New(s.app, log.New(io.Discard, "", 0), true, hosts, fake)
	sender.List = func(ctx context.Context) ([]db.Tenant, error) { return []db.Tenant{{ID: s.tenant, Slug: s.slug}}, nil }
	sender.Pass(ctx)
	failed := "Couldn't send to “Jobs sheet (demo)” (answered 400). An admin can fix it, then press Send again."
	again := `aria-label="Send again to Jobs sheet (demo)"`
	if body := page(admin, item); !strings.Contains(body, failed) || !strings.Contains(body, again) ||
		!strings.Contains(body, "couldn't send to “Jobs sheet (demo)”: answered 400. An admin must fix it") {
		t.Errorf("the admin's sidebar after a failed send: %s", problem(body))
	}
	if body := page(sales, item); !strings.Contains(body, failed) || strings.Contains(body, again) {
		t.Error("someone who isn't an admin sees Send again, or not the failure")
	}
	if !strings.Contains(page(admin, "/t/"+s.slug+"/"), "1 send failed") {
		t.Error("the admin's briefing doesn't count the failed send")
	}
	if leaves := page(admin, "/t/"+s.slug+"/settings/processes"); !strings.Contains(leaves, "Couldn't send (1), open the item to press Send again or Leave it:") ||
		!strings.Contains(leaves, item+`#sends">`+job.Number+"</a>") {
		t.Error("Leaves Seyal doesn't list the failed send with a link to its item")
	}

	// Send again: admins only; then it goes.
	var sendID string
	in(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM sends WHERE item_id = $1::uuid`, job.ID).Scan(&sendID)
	})
	if code, _, _ := s.post(sales, item+"/send-again", url.Values{"send": {sendID}}, nil); code != http.StatusForbidden {
		t.Errorf("Send again by someone who isn't an admin = %d, want 403", code)
	}
	code, body, _ := s.post(admin, item+"/send-again", url.Values{"send": {sendID}}, nil)
	if code != http.StatusOK || !strings.Contains(html.UnescapeString(body), "Queued again.") ||
		!strings.Contains(html.UnescapeString(body), "Sending to “Jobs sheet (demo)” in a moment.") {
		t.Errorf("Send again: %d %s", code, problem(body))
	}
	if code, _, _ := s.post(admin, item+"/send-again", url.Values{"send": {sendID}}, nil); code != http.StatusConflict {
		t.Errorf("Send again for a send that is waiting = %d, want 409", code)
	}
	fake.mu.Lock()
	fake.code, fake.reply = http.StatusOK, ""
	fake.mu.Unlock()
	if done := sender.Pass(ctx); done != 1 {
		t.Fatalf("after Send again: %d done", done)
	}
	if body := page(admin, item); !strings.Contains(body, "sent "+job.Number+" to “Jobs sheet (demo)” (it answered OK)") ||
		strings.Contains(body, again) {
		t.Errorf("after it went: %s", problem(body))
	}
	if strings.Contains(page(admin, "/t/"+s.slug+"/"), "send failed") {
		t.Error("the briefing still counts a send that went")
	}

	// Seyal pauses the app (too many sends in an hour): the admin's briefing says so.
	in(func(tx pgx.Tx) error {
		if err := ledger.Lock(ctx, tx); err != nil {
			return err
		}
		_, err := send.SeyalPause(ctx, tx, "jobs-sheet", send.WhyCap)
		return err
	})
	if !strings.Contains(page(admin, "/t/"+s.slug+"/"), "1 other app paused by Seyal") {
		t.Error("the admin's briefing doesn't say Seyal paused the app")
	}

	// The send fails after all, and the set-up file stops listing the app (made by hand here): Leaves
	// Seyal still lists the send, Send again says it can't help, and Leave it clears it.
	in(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sends SET done_at = NULL, gave_up_at = now(), last_reason = 'answered 400' WHERE id = $1`, sendID)
		return err
	})
	if err := db.WithTenant(ctx, s.owner, s.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM other_apps WHERE name = 'jobs-sheet'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if brief := page(admin, "/t/"+s.slug+"/"); !strings.Contains(brief, "1 send failed") || strings.Contains(brief, "paused by Seyal") {
		t.Error("the briefing after the app left the set-up file: want 1 send failed, and no paused app")
	}
	if leaves := page(admin, "/t/"+s.slug+"/settings/processes"); !strings.Contains(leaves, "Apps no longer in the set-up file") ||
		!strings.Contains(leaves, item+`#sends">`+job.Number+"</a>") {
		t.Error("Leaves Seyal doesn't list a failed send to an app the set-up file no longer lists")
	}
	if code, body, _ := s.post(admin, item+"/send-again", url.Values{"send": {sendID}}, nil); code != http.StatusConflict ||
		!strings.Contains(html.UnescapeString(body), "Sending this again can't work") {
		t.Errorf("Send again to an app no longer listed: %d %s", code, problem(body))
	}
	if code, _, _ := s.post(sales, item+"/leave-send", url.Values{"send": {sendID}}, nil); code != http.StatusForbidden {
		t.Errorf("Leave it by someone who isn't an admin = %d, want 403", code)
	}
	if code, body, _ := s.post(admin, item+"/leave-send", url.Values{"send": {sendID}}, nil); code != http.StatusOK ||
		!strings.Contains(html.UnescapeString(body), "Left. This send won't go.") ||
		!strings.Contains(html.UnescapeString(body), "chose to leave the send to “jobs-sheet”: it won't go") {
		t.Errorf("Leave it: %d %s", code, problem(body))
	}
	if strings.Contains(page(admin, "/t/"+s.slug+"/"), "send failed") {
		t.Error("the briefing still counts a send that was left")
	}
	if strings.Contains(page(admin, "/t/"+s.slug+"/settings/processes"), "Apps no longer in the set-up file") {
		t.Error("Leaves Seyal still lists a send that was left")
	}
	if code, _, _ := s.post(admin, item+"/leave-send", url.Values{"send": {sendID}}, nil); code != http.StatusConflict {
		t.Errorf("Leave it twice = %d, want 409", code)
	}
}
