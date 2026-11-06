//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/start"
)

// TestScheduleInSettings (D75, R): adding Monthly return to a business whose own client list lacks the
// details it needs offers one button that adds them and the process; the process page then says how it
// starts on its own, how many entries match now, and when it runs next; switched off, it says so; and a
// list without the yes/no detail turns the line into a warning.
func TestScheduleInSettings(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	ctx := context.Background()

	// This business's client list is older: it has neither "Files a monthly return" nor "Handled by".
	c, err := os.ReadFile("../../examples/masters/client.yaml")
	if err != nil {
		t.Fatal(err)
	}
	older := strings.Replace(string(c), "  - { key: monthly_return, label: Files a monthly return, type: bool }\n", "", 1)
	older = strings.Replace(older, "  - { key: handled_by, label: Handled by, type: person }\n", "", 1)
	if code, body, _ := s.post(l, base+"/settings/lists/client", url.Values{"source": {older}}, nil); code != http.StatusOK {
		t.Fatalf("change the client list = %d: %s", code, problem(body))
	}

	code, page, _ := s.post(l, base+"/settings/processes", url.Values{"template": {"monthly-return"}}, nil)
	for _, want := range []string{"Monthly return needs details your lists don't have yet:",
		// The banner at the top says so too (the advice is further down the page).
		`role="alert">Monthly return needs details your lists don&#39;t have yet. See below to add them.`,
		"Clients → Files a monthly return, Clients → Handled by", `name="lists" value="add"`,
		"Add them and use version 0.1", `href="` + base + `/settings/lists/client">See Clients`} {
		if code != http.StatusUnprocessableEntity || !strings.Contains(page, want) {
			t.Fatalf("adding it = %d, missing %q:\n%s", code, want, problem(page))
		}
	}
	if code, _, _ := s.get(l, base+"/settings/processes/monthly-return"); code != http.StatusNotFound {
		t.Error("Monthly return was added although the lists lack its details")
	}

	code, page, _ = s.post(l, base+"/settings/processes", url.Values{"template": {"monthly-return"}, "lists": {"add"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Monthly return is now in use.") {
		t.Fatalf("add them and use = %d: %s", code, problem(page))
	}
	_, page, _ = s.get(l, base+"/settings/lists/client")
	if !strings.Contains(page, "<b>Files a monthly return</b>") || !strings.Contains(page, "<b>Handled by</b>") {
		t.Errorf("the client list didn't get the details:\n%s", problem(page))
	}

	// The process page says how it starts on its own. The demo has two clients ticked; then a third is.
	zone, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(zone)
	sched := playbook.Schedule{Every: "month", Day: 1, Period: "previous"}
	cur := start.Due(sched, now, zone, 4)
	next := "Next run: " + start.Next(sched, cur, 4).RunDay.Format("2 Jan 2006") + "."
	if cur.RunDay.Format(time.DateOnly) == now.Format(time.DateOnly) {
		next = "The " + cur.Name + " run starts at Seyal's next check." // today is a run day
	}
	proc := base + "/settings/processes/monthly-return"
	_, page, _ = s.get(l, proc)
	if want := "Starts on its own: monthly on the 1st, for clients with “Files a monthly return” ticked (2 now). " + next; !strings.Contains(page, want) {
		t.Errorf("process page is missing %q:\n%s", want, around(page, "numbered"))
	}
	// Its details say the whole rule, without counts (as a draft's preview and "What changes" do).
	if want := "<h3>Starts on its own</h3><p class=\"why\">Monthly on the 1st, for the month just ended: for clients with “Files a monthly return” ticked, at most 30 a run."; !strings.Contains(page, want) {
		t.Errorf("process details are missing %q:\n%s", want, around(page, "Starts on its own</h3>"))
	}
	err = db.WithTenant(ctx, testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		mk, err := playbook.Kind(ctx, tx, "client")
		if err != nil {
			return err
		}
		for _, key := range []string{"deccan-motors"} {
			it, err := master.Get(ctx, tx, "client", key)
			if err != nil {
				return err
			}
			it.Fields["monthly_return"] = true
			if _, _, err := master.Save(ctx, tx, mk, key, it.Name, it.Fields, ledger.PersonActor(s.people[lakshmi])); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, page, _ = s.get(l, proc); !strings.Contains(page, "ticked (3 now)") {
		t.Errorf("after ticking a third the page says:\n%s", problem(page))
	}

	s.post(l, proc, url.Values{"action": {"disable"}}, nil)
	if _, page, _ = s.get(l, proc); !strings.Contains(page, "Switched off for new work, so it starts nothing now.") {
		t.Errorf("switched off, the page says:\n%s", problem(page))
	}
	s.post(l, proc, url.Values{"action": {"enable"}}, nil)

	// A list that lacks the yes/no detail all the same (written straight into the table: Settings refuses
	// it while the schedule uses it) stops the schedule, and the line says so.
	err = db.WithTenant(ctx, testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE master_kinds SET source = $1 WHERE tenant_id = current_tenant() AND kind = 'client'`,
			strings.Replace(string(c), "  - { key: monthly_return, label: Files a monthly return, type: bool }\n", "", 1))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, page, _ = s.get(l, proc); !strings.Contains(page, `<p class="callout warn" role="status">Schedule stopped: the Client list has no yes/no detail`) {
		t.Errorf("a stopped schedule isn't a warning:\n%s", problem(page))
	}
}

// around is the page's text near what, for a failure message.
func around(page, what string) string {
	i := strings.Index(page, what)
	if i < 0 {
		return "(" + what + " isn't on the page)"
	}
	return page[i:min(len(page), i+300)]
}
