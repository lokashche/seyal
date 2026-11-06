//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/setup"
	"seyal/internal/workitem"
)

// TestBriefing follows B (D75): the line at the top of My Work counts the same cards as the folders;
// "due today" is today in the business's time zone (not the server's, not UTC's) and comes from all
// four of the person's folders; each part opens what it counted; a person with nothing waiting reads
// "Nothing waiting on you today."; and admins also see work held by someone no longer active.
func TestBriefing(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	ctx := context.Background()
	base := "/t/" + s.slug

	// A time zone whose date isn't UTC's date right now, with an hour to spare either side: if "today"
	// were UTC's (or the server's), the cards due yesterday or tomorrow in the business would count.
	zoneName := "Pacific/Kiritimati" // UTC+14: already tomorrow from 10:00 UTC
	if time.Now().UTC().Hour() < 11 {
		zoneName = "Etc/GMT+12" // UTC-12: still yesterday until 12:00 UTC
	}
	zone, err := time.LoadLocation(zoneName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.owner.Exec(ctx, "UPDATE tenants SET timezone = $1 WHERE id = $2", zoneName, s.tenant); err != nil {
		t.Fatal(err)
	}
	local := time.Now().In(zone)
	today := local.Format(time.DateOnly)
	if today == time.Now().UTC().Format(time.DateOnly) {
		t.Fatalf("in %s today is UTC's day too; the test needs another zone", zoneName)
	}
	yesterday, tomorrow := local.AddDate(0, 0, -1).Format(time.DateOnly), local.AddDate(0, 0, 1).Format(time.DateOnly)

	for _, key := range []string{"asha", "ben", "dev", "chitra", "karthik"} {
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
	// start sends a new job to the Operations team, due on the day given.
	start := func(client, due string) string {
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
					"integrity_check": "pass", "client_data_due": due}}); err != nil {
				return err
			}
			_, err = workitem.Act(ctx, tx, it.ID, "send_to_ops", "", by("asha"))
			return err
		})
		return it.ID
	}
	browsers := map[string]*http.Client{}
	for _, key := range []string{"asha", "dev", "chitra", "karthik"} {
		browsers[key] = s.logIn(key)
	}
	type part struct{ text, link string }
	// line reads the person's briefing: its words, and each part with the address it opens.
	line := func(key, query string) (string, []part) {
		t.Helper()
		_, home, _ := s.get(browsers[key], base+"/"+query)
		m := regexp.MustCompile(`(?s)<p class="brief">(.*?)</p>`).FindStringSubmatch(home)
		if m == nil {
			t.Fatalf("%s's My Work has no briefing line:\n%s", key, problem(home))
		}
		var parts []part
		for _, a := range regexp.MustCompile(`<a href="([^"]*)">([^<]*)</a>`).FindAllStringSubmatch(m[1], -1) {
			parts = append(parts, part{html.UnescapeString(a[2]), html.UnescapeString(a[1])})
		}
		return html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], "")), parts
	}
	cardRe := regexp.MustCompile(`<a class="item[^"]*" href="` + regexp.QuoteMeta(base) + `/items/([0-9a-f-]+)`)
	// cards lists the items a list shows, and how many of them are marked New.
	cards := func(key, path string) (ids []string, marked int) {
		t.Helper()
		_, page, _ := s.get(browsers[key], path)
		for _, m := range cardRe.FindAllStringSubmatch(page, -1) {
			ids = append(ids, m[1])
		}
		slices.Sort(ids)
		return ids, strings.Count(page, `<span class="new">New</span>`)
	}
	// folderCount is the number the folder column shows beside a folder (0 when it shows none).
	folderCount := func(key, title string) string {
		t.Helper()
		_, home, _ := s.get(browsers[key], base+"/")
		if m := regexp.MustCompile(`<span class="ft">` + title + `</span><span class="n">(\d+)</span>`).FindStringSubmatch(home); m != nil {
			return m[1]
		}
		return "0"
	}
	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }

	// Before any work, nobody has anything waiting.
	if words, parts := line("dev", ""); words != "Nothing waiting on you today." || len(parts) != 0 {
		t.Errorf("Dev's line with no work = %q %v, want the all-zero words", words, parts)
	}

	// Dev's work: one in his own turn and one waiting for his approval, both due today; for his team,
	// one due today, one due yesterday (overdue) and one due tomorrow.
	mine := start("sundaram-textiles", today)
	if code, body, _ := s.post(browsers["dev"], base+"/items/"+mine+"/take", url.Values{"holder": {"team:ops"}, "via": {"button"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Dev's Take = %d: %s", code, problem(body))
	}
	approve := start("annapoorna-foods", today)
	in(func(tx pgx.Tx) error {
		if _, err := workitem.Take(ctx, tx, approve, "team:ops", "button", by("ben")); err != nil {
			return err
		}
		if _, err := workitem.SetFields(ctx, tx, approve, engine.Fields{"fee_agreed": 1500000.0}, by("ben")); err != nil {
			return err
		}
		_, err := workitem.Act(ctx, tx, approve, "submit", "", by("ben"))
		return err
	})
	team := start("deccan-motors", today)
	late := start("ganga-traders", yesterday)
	later := start("sundaram-textiles", tomorrow)

	day := local.Format("Mon 2 Jan")
	want := "Today, " + day + ": 1 overdue · 3 due today · 4 new for you · 1 waiting on your approval"
	words, parts := line("dev", "")
	if words != want {
		t.Fatalf("Dev's line = %q, want %q", words, want)
	}
	// The counts are the folders' counts, and each part opens exactly the cards it counted.
	if n := folderCount("dev", "Overdue"); n != "1" {
		t.Errorf("Dev's Overdue folder shows %s, the line says 1", n)
	}
	if n := folderCount("dev", "Needs my approval"); n != "1" {
		t.Errorf("Dev's Needs my approval folder shows %s, the line says 1", n)
	}
	opens := map[string][]string{
		"1 overdue":                  {late},
		"3 due today":                sorted(mine, approve, team),
		"4 new for you":              sorted(approve, team, late, later),
		"1 waiting on your approval": {approve},
	}
	for _, p := range parts {
		ids, marked := cards("dev", p.link)
		if !slices.Equal(ids, opens[p.text]) {
			t.Errorf("Dev's %q opens %s with %v, want %v", p.text, p.link, ids, opens[p.text])
		}
		if p.text == "4 new for you" && marked != 4 {
			t.Errorf("Dev's New list marks %d cards New, want 4", marked)
		}
	}
	// Due today comes from all four folders: his own turn, his approval and his team.
	for folder, id := range map[string]string{"turn": mine, "approval": approve, "team": team} {
		if ids, _ := cards("dev", base+"/?f="+folder); !slices.Contains(ids, id) {
			t.Errorf("Dev's %q folder doesn't hold the card due today it should (%s): %v", folder, id, ids)
		}
	}
	if _, page, _ := s.get(browsers["dev"], base+"/?f=today"); !strings.Contains(page, "<b>Due today</b>") {
		t.Errorf("the due-today list has the wrong title:\n%s", problem(page))
	}

	// A process sub-folder narrows the line, and its parts open just that process's cards.
	words, parts = line("dev", "?p=job-evaluation")
	if words != want || len(parts) == 0 || !strings.Contains(parts[0].link, "p=job-evaluation") {
		t.Errorf("Dev's line in Job evaluation = %q %v, want %q with links that keep the process", words, parts, want)
	}
	if _, page, _ := s.get(browsers["dev"], base+"/?f=overdue&p=job-evaluation"); !strings.Contains(page, "<b>Overdue · Job evaluation</b>") {
		t.Errorf("the line's overdue list for one process has the wrong title:\n%s", problem(page))
	}
	// ?p= alone is all the process's open work, although Dev's first folder with cards (the one My Work
	// opens without ?f=) is Needs my approval, which may be narrowed to a process.
	if ids, _ := cards("dev", base+"/?p=job-evaluation"); !slices.Equal(ids, sorted(mine, approve, team, late, later)) {
		t.Errorf("Dev's ?p=job-evaluation shows %v, want all five open jobs", ids)
	}
	if _, page, _ := s.get(browsers["dev"], base+"/?p=job-evaluation"); !strings.Contains(page, "<b>Job evaluation</b>") {
		t.Errorf("?p=job-evaluation alone should be titled Job evaluation:\n%s", problem(page))
	}

	// Opening a card counts as seeing it: one fewer New.
	s.get(browsers["dev"], base+"/items/"+team)
	if words, _ := line("dev", ""); !strings.Contains(words, "3 new for you") {
		t.Errorf("after Dev opened one New card his line = %q, want 3 new for you", words)
	}

	// Someone with nothing for them, though the business has open work.
	if words, parts := line("karthik", ""); words != "Nothing waiting on you today." || len(parts) != 0 {
		t.Errorf("Karthik's line = %q %v, want the all-zero words", words, parts)
	}

	// Admins also see work held by someone no longer active; Dev, who isn't an admin, doesn't.
	held := start("ganga-traders", tomorrow)
	in(func(tx pgx.Tx) error {
		if _, err := workitem.Take(ctx, tx, held, "team:ops", "button", by("ben")); err != nil {
			return err
		}
		_, err := people.SetActive(ctx, tx, s.people["ben"], false, ledger.SystemActor)
		return err
	})
	words, parts = line("chitra", "")
	i := slices.IndexFunc(parts, func(p part) bool { return p.text == "1 with someone no longer active" })
	if i < 0 {
		t.Fatalf("Chitra's line = %q, want it to count Ben's job (he is no longer active)", words)
	}
	if ids, _ := cards("chitra", parts[i].link); !slices.Contains(ids, held) {
		t.Errorf("Chitra's %q opens %s without Ben's job: %v", parts[i].text, parts[i].link, ids)
	}
	if words, _ := line("dev", ""); strings.Contains(words, "no longer active") {
		t.Errorf("Dev, not an admin, sees %q", words)
	}
	s.tablesMatchLedger(t) // the line changes nothing
}

// TestBriefingAdmin: the admins' parts of the line (D82), each counted once and opening what it counted:
// work with someone no longer active, with someone no longer in the step's role, that nobody can take,
// that Seyal couldn't escalate, and a schedule that is stopped. With a process open, the For my team
// parts open For my team for that process only.
func TestBriefingAdmin(t *testing.T) {
	s := newSiteFrom(t, "../../examples/playbooks", []string{"job-evaluation", "monthly-return"})
	ctx := context.Background()
	base := "/t/" + s.slug
	var err error
	for _, key := range []string{"asha", "ben", "dev", "chitra"} {
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
	// start starts a job (due in a month, so no day counts), sends it to Operations when send is true,
	// and lets taker take it when one is given.
	due := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	start := func(client string, send bool, taker string) string {
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
					"integrity_check": "pass", "client_data_due": due}}); err != nil || !send {
				return err
			}
			if _, err = workitem.Act(ctx, tx, it.ID, "send_to_ops", "", by("asha")); err != nil || taker == "" {
				return err
			}
			_, err = workitem.Take(ctx, tx, it.ID, "team:ops", "button", by(taker))
			return err
		})
		return it.ID
	}
	gone := start("sundaram-textiles", true, "ben")
	lost := start("annapoorna-foods", true, "dev")
	stuck := start("deccan-motors", true, "dev")
	nobody := start("ganga-traders", true, "")
	other := start("sundaram-textiles", false, "") // still with Asha: not for an admin to step in
	in(func(tx pgx.Tx) error {
		// Nobody who can take work is in Owner (Elan has no login), so the escalation can't happen.
		if _, err := people.Revoke(ctx, tx, s.people["chitra"], "owner", ledger.SystemActor); err != nil {
			return err
		}
		if _, err := people.SetActive(ctx, tx, s.people[lakshmi], false, ledger.SystemActor); err != nil {
			return err
		}
		it, err := workitem.Get(ctx, tx, stuck)
		if err != nil {
			return err
		}
		zone, _ := time.LoadLocation("Asia/Kolkata")
		did, err := workitem.Escalate(ctx, tx, stuck, workitem.Chase{StepSince: it.StepSince, Reminders: it.Reminders, Days: 4,
			Now: time.Now(), Zone: zone})
		if err != nil {
			return err
		}
		if did == "" {
			t.Error("the escalation did nothing")
		}
		// Dev leaves Operations, and Ben and Priya leave, so Operations has nobody left.
		if _, err := people.Revoke(ctx, tx, s.people["dev"], "ops", ledger.SystemActor); err != nil {
			return err
		}
		for _, key := range []string{"ben", priya} {
			if _, err := people.SetActive(ctx, tx, s.people[key], false, ledger.SystemActor); err != nil {
				return err
			}
		}
		return nil
	})
	s.tablesMatchLedger(t)
	// The client list loses its yes/no detail (written straight into the table: Settings refuses it
	// while the schedule uses it), so Monthly return's schedule stops.
	c, err := os.ReadFile("../../examples/masters/client.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE master_kinds SET source = $1 WHERE tenant_id = current_tenant() AND kind = 'client'`,
			strings.Replace(string(c), "  - { key: monthly_return, label: Files a monthly return, type: bool }\n", "", 1))
		return err
	})

	ch := s.logIn("chitra")
	type part struct{ text, link string }
	// line reads Chitra's briefing: its words without "Today, <day>: ", and each part with its address.
	line := func(query string) (string, []part) {
		t.Helper()
		_, home, _ := s.get(ch, base+"/"+query)
		m := regexp.MustCompile(`(?s)<p class="brief">(.*?)</p>`).FindStringSubmatch(home)
		if m == nil {
			t.Fatalf("Chitra's My Work has no briefing line:\n%s", problem(home))
		}
		var parts []part
		for _, a := range regexp.MustCompile(`<a href="([^"]*)">([^<]*)</a>`).FindAllStringSubmatch(m[1], -1) {
			parts = append(parts, part{html.UnescapeString(a[2]), html.UnescapeString(a[1])})
		}
		words := html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], ""))
		_, words, _ = strings.Cut(words, ": ")
		return words, parts
	}
	cardRe := regexp.MustCompile(`<a class="item[^"]*" href="` + regexp.QuoteMeta(base) + `/items/([0-9a-f-]+)`)
	cards := func(path string) (ids []string, page string) {
		t.Helper()
		_, page, _ = s.get(ch, path)
		for _, m := range cardRe.FindAllStringSubmatch(page, -1) {
			ids = append(ids, m[1])
		}
		slices.Sort(ids)
		return ids, page
	}
	flagged := []string{gone, lost, stuck, nobody}
	slices.Sort(flagged)

	// The four cards are in Chitra's For my team, never seen by her, so they are New for her too (D81).
	team := "4 new for you · 1 with someone no longer active · 1 with someone no longer in the step's role · 1 that nobody can take · 1 couldn't escalate"
	words, parts := line("")
	if want := team + " · 1 schedule held back in Monthly return"; words != want {
		t.Fatalf("Chitra's line = %q, want %q", words, want)
	}
	for _, p := range parts {
		if p.text == "1 schedule held back in Monthly return" {
			if p.link != base+"/settings/processes/monthly-return" {
				t.Errorf("%q links to %q, want Monthly return's Settings page", p.text, p.link)
			} else if _, page, _ := s.get(ch, p.link); !strings.Contains(page, "Schedule stopped: the Client list has no yes/no detail") {
				t.Errorf("%q opens a page without the warning:\n%s", p.text, problem(page))
			}
			continue
		}
		if ids, _ := cards(p.link); !slices.Equal(ids, flagged) {
			t.Errorf("%q opens %s with %v, want For my team's %v", p.text, p.link, ids, flagged)
		}
	}
	if ids, _ := cards(base + "/?f=all"); !slices.Contains(ids, other) || len(ids) != 5 {
		t.Errorf("All open work should hold all five jobs, Asha's too: %v", ids)
	}

	// With a process open the line counts it only (Monthly return's schedule isn't Job evaluation's),
	// and each part opens For my team for that process, not all its open work.
	words, parts = line("?p=job-evaluation")
	if words != team || len(parts) != 5 {
		t.Fatalf("Chitra's line in Job evaluation = %q, want %q", words, team)
	}
	for _, p := range parts {
		ids, page := cards(p.link)
		title := "<b>For my team · Job evaluation</b>"
		if p.text == "4 new for you" {
			title = "<b>New for you · Job evaluation</b>"
		}
		if !strings.Contains(p.link, "p=job-evaluation") || !slices.Equal(ids, flagged) || !strings.Contains(page, title) {
			t.Errorf("in Job evaluation %q opens %s with %v, want %s with %v", p.text, p.link, ids, title, flagged)
		}
	}
}
