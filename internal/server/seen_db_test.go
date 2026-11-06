//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/setup"
	"seyal/internal/workitem"
)

// TestNewMark follows M7 (D75) on the shipped samples: a card is New for whoever it came to (a team, a
// pass, a move, an escalation, a step that waits for their approval) until they open it or act on it;
// not after their own Take; a reminder doesn't make it New; each person of a team has their own mark;
// and right after the first-day rows of migration 00023 nothing is New for anyone.
func TestNewMark(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	ctx := context.Background()
	base := "/t/" + s.slug
	for _, key := range []string{"asha", "ben", "dev", "chitra", "elan"} {
		var err error
		if s.passwords[key], err = setup.NewPassword(ctx, s.app, s.mail(key), s.tenant); err != nil {
			t.Fatal(err)
		}
	}
	by := func(key string) ledger.Actor { return ledger.PersonActor(s.people[key]) }
	in := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, s.app, s.tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	start := func(client string) string {
		t.Helper()
		var it workitem.Item
		in(func(tx pgx.Tx) error {
			var pv string
			if err := tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'job-evaluation'`).Scan(&pv); err != nil {
				return err
			}
			var err error
			if it, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by("asha"),
				Fields: engine.Fields{"client": client, "nature_of_work": "annual-accounts", "reason": "Accounts for the year",
					"integrity_check": "pass"}}); err != nil {
				return err
			}
			_, err = workitem.Act(ctx, tx, it.ID, "send_to_ops", "", by("asha"))
			return err
		})
		return it.ID
	}
	browsers := map[string]*http.Client{}
	for _, key := range []string{"asha", "ben", "dev", "chitra", "elan"} {
		browsers[key] = s.logIn(key)
	}
	// card says whether the person's folder shows the item's card, and whether it is marked New.
	card := func(key, folder, id string) (shown, isNew bool) {
		t.Helper()
		_, home, _ := s.get(browsers[key], base+"/?f="+folder)
		m := regexp.MustCompile(`href="` + regexp.QuoteMeta(base+"/items/"+id) + `[^"]*"[^>]*>\s*<span class="top">(<span class="new">New</span>)?`).
			FindStringSubmatch(home)
		return m != nil, m != nil && m[1] != ""
	}
	want := func(key, folder, id string, wantNew bool, why string) {
		t.Helper()
		shown, isNew := card(key, folder, id)
		if !shown {
			t.Fatalf("%s: %s's %q folder doesn't show the card", why, key, folder)
		}
		if isNew != wantNew {
			t.Errorf("%s: %s's card New = %v, want %v", why, key, isNew, wantNew)
		}
	}

	job := start("annapoorna-foods")
	jobPage := base + "/items/" + job
	want("ben", "team", job, true, "sent to the Operations team")
	want("dev", "team", job, true, "sent to the Operations team")
	want("asha", "all", job, false, "her own work, waiting on others")

	// Ben takes it from his card (the form sends him on to the item; he doesn't follow it here).
	if code, body, _ := s.post(browsers["ben"], jobPage+"/take", url.Values{"holder": {"team:ops"}, "via": {"button"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Ben's Take = %d: %s", code, problem(body))
	}
	want("ben", "turn", job, false, "his own Take")

	// Ben passes it to Dev: New for Dev until he opens it. On the item's own page its card isn't New.
	if code, body, _ := s.post(browsers["ben"], jobPage+"/pass", url.Values{"to": {"person:" + s.people["dev"]},
		"note": {"please do this one"}, "holder": {"person:" + s.people["ben"]}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Ben's pass = %d: %s", code, problem(body))
	}
	want("dev", "turn", job, true, "passed to him")
	if _, page, _ := s.get(browsers["dev"], jobPage+"?f=turn"); strings.Contains(page, `<span class="new">`) {
		t.Error("the open item's own card is marked New")
	}
	want("dev", "turn", job, false, "after he opened it")

	// Chitra moves Dev's open work to Ben (M3): New for Ben, who saw it last when he took it. He opens
	// the item while the move is still being made: his page shows it with Dev, so the move, which
	// commits after his page, is still New (the seen time is what the page showed, not the clock).
	in(func(tx pgx.Tx) error {
		_, err := workitem.Reassign(ctx, tx, []string{job}, s.people["dev"], s.people["ben"], "", by("chitra"))
		if err == nil {
			s.get(browsers["ben"], jobPage)
		}
		return err
	})
	want("ben", "turn", job, true, "moved to him while he had its page open")
	s.get(browsers["ben"], jobPage)
	want("ben", "turn", job, false, "after he opened it")

	// A reminder doesn't change hands, so it doesn't make the card New; an escalation does, for each
	// person of the Owner team on their own.
	zone, _ := time.LoadLocation("Asia/Kolkata")
	chase := func(do func(context.Context, pgx.Tx, string, workitem.Chase) (string, error), days int) {
		t.Helper()
		in(func(tx pgx.Tx) error {
			it, err := workitem.Get(ctx, tx, job)
			if err != nil {
				return err
			}
			did, err := do(ctx, tx, job, workitem.Chase{StepSince: it.StepSince, Reminders: it.Reminders, Days: days,
				Now: time.Now(), Zone: zone})
			if did == "" && err == nil {
				t.Errorf("nothing was done on day %d", days)
			}
			return err
		})
	}
	chase(workitem.Remind, 2)
	want("ben", "turn", job, false, "after a reminder")
	chase(workitem.Escalate, 4)
	want("chitra", "team", job, true, "escalated to the Owner team")
	want("elan", "team", job, true, "escalated to the Owner team")
	s.get(browsers["chitra"], jobPage)
	want("chitra", "team", job, false, "after she opened it")
	want("elan", "team", job, true, "Chitra opened it, but Elan hasn't")

	// A second job reaches its approvals: New for Asha, whose approval it waits for, and for Dev, who
	// passed it to Ben before: Ben's submit is a new step, not Dev's own hand-over.
	second := start("sundaram-textiles")
	in(func(tx pgx.Tx) error {
		_, err := workitem.Take(ctx, tx, second, "team:ops", "button", by("dev"))
		return err
	})
	if code, body, _ := s.post(browsers["dev"], base+"/items/"+second+"/pass", url.Values{"to": {"person:" + s.people["ben"]},
		"holder": {"person:" + s.people["dev"]}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Dev's pass = %d: %s", code, problem(body))
	}
	in(func(tx pgx.Tx) error {
		if _, err := workitem.SetFields(ctx, tx, second, engine.Fields{"fee_agreed": 1500000.0}, by("ben")); err != nil {
			return err
		}
		_, err := workitem.Act(ctx, tx, second, "submit", "", by("ben"))
		return err
	})
	want("asha", "approval", second, true, "waiting for her approval")
	want("dev", "approval", second, true, "waiting for his approval, after he passed it on")

	// The first day of M7: migration 00023 marks every item seen by every person, business by business
	// (the owner runs its function here, as the migration does). Then nothing is New for anyone.
	var rows int
	if err := s.owner.QueryRow(ctx, "SELECT mark_all_seen($1)", s.tenant).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("mark_all_seen = %d rows, %v", rows, err)
	}
	for key, c := range browsers {
		for _, f := range []string{"approval", "overdue", "turn", "team", "all"} {
			if _, home, _ := s.get(c, base+"/?f="+f); strings.Contains(home, `<span class="new">`) {
				t.Errorf("right after the first-day rows, %s's %q folder shows a New card", key, f)
			}
		}
	}
	want("elan", "team", job, false, "after the first-day rows")
	want("asha", "approval", second, false, "after the first-day rows")
	want("dev", "approval", second, false, "after the first-day rows")
	s.tablesMatchLedger(t) // seen times aren't in the ledger, and opening items changed no work
}
