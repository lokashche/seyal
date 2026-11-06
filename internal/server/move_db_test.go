//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/people"
	"seyal/internal/playbook"
	"seyal/internal/setup"
	"seyal/internal/workitem"
)

// moveForm finds the list's form: who it moves from and to, and the items "Move all" sends.
var (
	moveFrom  = regexp.MustCompile(`<input type="hidden" name="move_from" value="([^"]+)"><input type="hidden" name="move_to" value="([^"]+)">`)
	moveItems = regexp.MustCompile(`<input type="hidden" name="item" value="([^"]+)">`)
)

// TestMoveSomeonesWork follows the story (D75, M3) on the whole demo business: Ben holds a job in With
// ops and an Other work item. Chitra (an admin) types or says "Ben is on leave Thu and Fri, move his work
// to Dev" on Tell Seyal and gets the list with Move all, Pick and No; nothing is saved until she presses.
// Asha (not an admin) is refused. The other shapes of the words work, a question and an ordinary
// sentence don't, and two people called Ben get "Which one do you mean?". An item Asha couldn't work
// starts unticked. Settings → People and My Work point admins at work held by someone who left or lost
// the step's role. After the press each thread says who moved it from whom, and Dev's cards say why.
func TestMoveSomeonesWork(t *testing.T) {
	s := newDemoSite(t)
	ctx := context.Background()
	base := "/t/" + s.slug
	for _, key := range []string{"asha", "ben", "dev", "chitra"} {
		var err error
		if s.passwords[key], err = setup.NewPassword(ctx, s.app, s.mail(key), s.tenant); err != nil {
			t.Fatal(err)
		}
	}
	in := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, s.app, s.tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	by := func(key string) ledger.Actor { return ledger.PersonActor(s.people[key]) }
	version := func(tx pgx.Tx, process string) (id string, err error) {
		err = tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = $1`, process).Scan(&id)
		return id, err
	}

	// Ben's open work: a job he took in With ops, and Other work he does ("Who does it").
	var job, other workitem.Item
	in(func(tx pgx.Tx) error {
		pv, err := version(tx, "job-evaluation")
		if err != nil {
			return err
		}
		if job, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: by("asha"),
			Fields: engine.Fields{"client": "annapoorna-foods", "nature_of_work": "annual-accounts", "reason": "Accounts for the year",
				"integrity_check": "pass"}}); err != nil {
			return err
		}
		if _, err = workitem.Act(ctx, tx, job.ID, "send_to_ops", "", by("asha")); err != nil {
			return err
		}
		if job, err = workitem.Take(ctx, tx, job.ID, "team:ops", "button", by("ben")); err != nil {
			return err
		}
		ow, err := version(tx, "other-work")
		if err != nil {
			return err
		}
		if other, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: ow, Via: "chat", By: by("asha"),
			Fields: engine.Fields{"what": "Renew the office internet plan"}}); err != nil {
			return err
		}
		other, err = workitem.SetFields(ctx, tx, other.ID, engine.Fields{"who": s.people["ben"]}, by("asha"))
		return err
	})
	if job.AssigneeID != s.people["ben"] || other.AssigneeID != s.people["ben"] {
		t.Fatalf("set-up: the job is with %q and Other work with %q, want Ben for both", job.AssigneeID, other.AssigneeID)
	}

	c, a := s.logIn("chitra"), s.logIn("asha")
	tell := func(b *http.Client, text string) string {
		t.Helper()
		code, page, _ := s.post(b, base+"/tell", url.Values{"playbook": {"auto"}, "text": {text}}, nil)
		if code != http.StatusOK {
			t.Fatalf("tell %q = %d: %s", text, code, problem(page))
		}
		return html.UnescapeString(page)
	}
	// Messages and work events (logging in is recorded too, but it isn't what this is about).
	saved := func() int {
		return s.count("SELECT count(*) FROM messages") + s.count("SELECT count(*) FROM ledger WHERE subject_type <> 'person'")
	}
	before := saved()

	// Asha isn't an admin: refused, and nothing is saved.
	if page := tell(a, "Move Ben's work to Dev"); !strings.Contains(page,
		"Only an admin can move someone else's work. You can pass your own items one by one.") || strings.Contains(page, "open items") {
		t.Errorf("Asha should be refused:\n%s", problem(page))
	}

	// Chitra gets the list: nothing is ticked off yet and nothing is saved.
	list := tell(c, "Ben is on leave Thu and Fri, move his work to Dev")
	for _, want := range []string{"Ben has 2 open items:", "<b>" + job.Number + "</b>", "With ops", "<b>" + other.Number + "</b>",
		"Renew the office internet plan · Open", "Move all 2 to Dev", ">Pick</button>", ">No</a>", "Note for Dev (optional)",
		"Everyone who can open these items sees this note.", "<p>Ben is on leave Thu and Fri, move his work to Dev</p>"} {
		if !strings.Contains(list, want) {
			t.Errorf("the list should have %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "on leave Thu and Fri</") || strings.Contains(list, `name="note" maxlength="500" value=`) {
		t.Error("the note should start empty, never filled from the message")
	}
	if got := saved(); got != before {
		t.Errorf("the refusal and the list saved %d rows; want nothing", got-before)
	}

	// The other shapes give the same list; a question and an ordinary sentence don't.
	for _, text := range []string{"Move the work from Ben to Dev.", "Give all of Ben's jobs to Dev"} {
		if page := tell(c, text); !strings.Contains(page, "Ben has 2 open items:") {
			t.Errorf("%q should give the list:\n%s", text, problem(page))
		}
	}
	if page := tell(c, "Move Ben's work to Dev?"); strings.Contains(page, "open items:") {
		t.Errorf("a question isn't a command:\n%s", page)
	}
	if page := tell(c, "Hand the Sundaram documents to Dev, Ben collected them"); strings.Contains(page, "open items:") ||
		!confirmForm.MatchString(page) {
		t.Errorf("an ordinary sentence should start new work as usual (a process to confirm):\n%s", problem(page))
	}
	if page := tell(c, "Move Ben's work to Deb"); !strings.Contains(page, "I don't know Deb here.") {
		t.Errorf("an unknown name should be said:\n%s", problem(page))
	}
	// A name from the lists isn't a person, so "Sundaram's papers" is new work, also for an admin; so is
	// a sentence about someone who isn't here.
	for _, text := range []string{"Pass Sundaram's papers to Dev", "Move the file from Sundaram to Dev",
		"My colleague Ben is away, move his work to Dev"} {
		if page := tell(c, text); strings.Contains(page, "I don't know") || !confirmForm.MatchString(page) {
			t.Errorf("%q should start new work as usual (a process to confirm):\n%s", text, problem(page))
		}
	}
	// Small words after the receiver's name aren't part of it.
	for _, text := range []string{"Move Ben's work to Dev please", "Move Ben's work to Dev!", "Move Ben's work to Dev, thanks"} {
		if page := tell(c, text); !strings.Contains(page, "Ben has 2 open items:") {
			t.Errorf("%q should give the list:\n%s", text, problem(page))
		}
	}

	// Spoken, the words give the same list with the same buttons, and the recording isn't kept.
	words := "Ben is on leave, move his work to Dev"
	s.ai.SetSpeech(ai.Speech{Name: "fake:test", Model: &ai.FakeSpeech{Answer: func(ai.Audio) (ai.Transcript, error) {
		return ai.Transcript{Text: words, Seconds: 2}, nil
	}}})
	audio := ai.FakeRecording(2)
	files, _ := s.kept()
	if code, ans := s.speak(c, "", audio, "audio/webm"); code != http.StatusOK || ans.Token == "" {
		t.Errorf("voice = %d %+v", code, ans)
	} else if code, page, _ := s.sendSpoken(c, base+"/tell", ans.Text, ans.Token, audio, "playbook", "auto"); code != http.StatusOK ||
		!strings.Contains(html.UnescapeString(page), "Move all 2 to Dev") || !strings.Contains(page, ">Pick</button>") {
		t.Errorf("spoken = %d, want the same list and buttons:\n%s", code, problem(page))
	}
	if now, _ := s.kept(); now != files {
		t.Errorf("the spoken move kept %d recordings; want none", now-files)
	}

	// To Asha (Sales), the job in With ops starts unticked: she could only pass it on.
	_, page, _ := s.get(c, base+"/?view=tell&move_from="+s.people["ben"]+"&move_to="+s.people["asha"])
	page = html.UnescapeString(page)
	if !strings.Contains(page, "Asha isn't in Operations, so they could only pass this on.") || !strings.Contains(page, "Move 1 to Asha") ||
		len(moveItems.FindAllStringSubmatch(page, -1)) != 1 || moveItems.FindStringSubmatch(page)[1] != other.ID {
		t.Errorf("to Asha, only Other work should be ticked:\n%s", page)
	}
	if _, page, _ = s.get(c, base+"/?view=tell&move_from="+s.people["ben"]+"&move_to="+s.people["asha"]+"&pick=1"); !strings.Contains(page, "Move these") ||
		!strings.Contains(page, `name="item" value="`+job.ID+`"`) {
		t.Errorf("Pick should let every item be ticked:\n%s", page)
	}

	// A link never fills in the note or ticks boxes: anyone could send Chitra a link with words already
	// typed into her permanent note.
	forged := base + "/?" + url.Values{"view": {"tell"}, "move_from": {s.people["ben"]}, "move_to": {s.people["dev"]},
		"item": {other.ID}, "note": {"Owner approved the revised fee"}, "pick": {"1"}, "picked": {"1"}}.Encode()
	if _, page, _ = s.get(c, forged); !strings.Contains(page, "Move these") || strings.Contains(page, "Owner approved the revised fee") ||
		!strings.Contains(page, `value="`+job.ID+`" checked>`) { // both start ticked for Dev; the link's item= is ignored
		t.Errorf("a link should open the list with an empty note and its usual boxes:\n%s", page)
	}

	// Pick is a button of the list's form (a POST, show=pick), so the note typed comes along, and
	// nothing moves. A refused press shows the list again under the reason, with the boxes as they were
	// ticked and the note.
	code, page, _ := s.post(c, base+"/move", url.Values{"move_from": {s.people["ben"]}, "move_to": {s.people["dev"]},
		"item": {job.ID, other.ID}, "note": {"Back on Monday"}, "show": {"pick"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "Move these") || !strings.Contains(page, `value="Back on Monday"`) ||
		strings.Contains(page, "Moved ") {
		t.Errorf("Pick = %d, want the list with boxes and the note kept, nothing moved:\n%s", code, page)
	}
	code, page, _ = s.post(c, base+"/move", url.Values{"move_from": {s.people["ben"]}, "move_to": {s.people["dev"]},
		"pick": {"1"}, "note": {"Back on Monday"}}, nil)
	page = html.UnescapeString(page)
	if code != http.StatusUnprocessableEntity || !strings.Contains(page, "Tick at least one item to move") ||
		!strings.Contains(page, "Ben has 2 open items:") || !strings.Contains(page, "Move these") ||
		!strings.Contains(page, `value="Back on Monday"`) || strings.Contains(page, `value="`+job.ID+`" checked>`) ||
		strings.Contains(page, `value="`+other.ID+`" checked>`) {
		t.Errorf("a refused press = %d, want the list again, nothing ticked, and the note:\n%s", code, page)
	}
	// A press with more than workitem.MaxMove items (a forged form) is refused before anything moves,
	// and the list comes back with Other work still ticked as it was and the note.
	huge := url.Values{"move_from": {s.people["ben"]}, "move_to": {s.people["dev"]}, "pick": {"1"}, "item": {other.ID},
		"note": {"Back on Monday"}}
	for i := range workitem.MaxMove {
		huge.Add("item", fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
	}
	code, page, _ = s.post(c, base+"/move", huge, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(page, "more than 500 items in one go") ||
		!strings.Contains(page, `value="`+other.ID+`" checked>`) || strings.Contains(page, `value="`+job.ID+`" checked>`) ||
		!strings.Contains(page, `value="Back on Monday"`) {
		t.Errorf("a press of %d items = %d, want it refused, with the boxes kept as ticked and the note:\n%s", len(huge["item"]), code, page)
	}
	in(func(tx pgx.Tx) error {
		it, err := workitem.Get(ctx, tx, job.ID)
		if err == nil && it.AssigneeID != s.people["ben"] {
			t.Errorf("after the refused presses the job is with %q, want Ben still", it.AssigneeID)
		}
		return err
	})

	// Settings → People and My Work point admins at Ben's work once he lost the Ops role, then left.
	peoplePage := func() string {
		t.Helper()
		_, page, _ := s.get(c, base+"/settings/people")
		return html.UnescapeString(page)
	}
	if page := peoplePage(); !strings.Contains(page, "Holds 2 open items.") || !strings.Contains(page, `name="move_from" value="`+s.people["ben"]+`"`) ||
		!strings.Contains(page, "Move them…") {
		t.Errorf("Settings → People should say Ben holds 2 open items, with Move them…:\n%s", page)
	}
	in(func(tx pgx.Tx) error {
		_, err := people.Revoke(ctx, tx, s.people["ben"], "ops", ledger.SystemActor)
		return err
	})
	if page := peoplePage(); !strings.Contains(page, "Ben holds 2 open items, 1 of them in a step they can no longer work.") {
		t.Errorf("Settings → People should say Ben holds an item in a step he can no longer work:\n%s", page)
	}
	if _, home, _ := s.get(c, base+"/?f=team"); !strings.Contains(html.UnescapeString(home), "With Ben, who is no longer in Operations: pass it on") {
		t.Errorf("Chitra's For my team should flag the job Ben can no longer work:\n%s", home)
	}
	in(func(tx pgx.Tx) error {
		_, err := people.SetActive(ctx, tx, s.people["ben"], false, ledger.SystemActor)
		return err
	})
	if page := peoplePage(); !strings.Contains(page, "Ben is disabled and still holds 2 open items.") {
		t.Errorf("Settings → People should say Ben is disabled and still holds his work:\n%s", page)
	}
	if _, home, _ := s.get(c, base+"/?f=team"); strings.Count(html.UnescapeString(home), "With Ben, who is no longer active: pass it on") != 2 {
		t.Errorf("Chitra's For my team should flag both items of Ben, who is no longer active:\n%s", home)
	}

	// Two people called Ben: "Which one do you mean?", one button each.
	if code, page, _ := s.post(s.logIn(lakshmi), base+"/settings/people", url.Values{"name": {"Ben Kumar"},
		"email": {s.mail("benkumar")}, "role": {"ops"}}, nil); code != http.StatusOK {
		t.Fatalf("adding Ben Kumar = %d: %s", code, problem(page))
	}
	if page := tell(c, "Move Ben's work to Dev"); !strings.Contains(page, "Which one do you mean?") ||
		!strings.Contains(page, `name="move_from" value="`+s.people["ben"]+`"`) || !strings.Contains(page, ">Ben Kumar</button>") {
		t.Errorf("two Bens should get a which-one question with a button each:\n%s", page)
	}

	// The press: everything moves to Dev, recorded as Chitra, with the fixed note.
	m := moveFrom.FindStringSubmatch(list)
	ids := moveItems.FindAllStringSubmatch(list, -1)
	if m == nil || len(ids) != 2 {
		t.Fatalf("the list's form is missing:\n%s", list)
	}
	form := url.Values{"move_from": {m[1]}, "move_to": {m[2]}, "note": {""}}
	for _, id := range ids {
		form.Add("item", id[1])
	}
	code, page, _ = s.post(c, base+"/move", form, nil)
	if code != http.StatusOK || !strings.Contains(page, "Moved 2 items to Dev.") {
		t.Fatalf("the press = %d:\n%s", code, problem(page))
	}
	d := s.logIn("dev")
	for _, it := range []workitem.Item{job, other} {
		_, page, _ := s.get(d, base+"/items/"+it.ID)
		if !strings.Contains(html.UnescapeString(page), "Chitra: moved it from Ben to Dev (as an admin, with 1 other item): Moved here from Ben") {
			t.Errorf("%s's thread should say Chitra moved it:\n%s", it.Number,
				strings.Join(regexp.MustCompile(`<div class="evt">.*</div>`).FindAllString(html.UnescapeString(page), -1), "\n"))
		}
	}
	if _, home, _ := s.get(d, base+"/?f=all"); strings.Count(html.UnescapeString(home), "From Chitra: Moved here from Ben") != 2 {
		t.Errorf("Dev's cards should say who moved them and why:\n%s", home)
	}
	// Pressing the same list again moves nothing: the items aren't with Ben any more.
	if _, page, _ := s.post(c, base+"/move", form, nil); !strings.Contains(html.UnescapeString(page), "Nothing was moved.") ||
		!strings.Contains(html.UnescapeString(page), job.Number+" was not moved: it is with Dev now.") {
		t.Errorf("an old list should move nothing and say why:\n%s", problem(page))
	}

	// With no process switched on for new work, open work can still move: typed, and from Settings → People.
	in(func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT playbook_id FROM tenant_playbooks WHERE active`)
		processes, err := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, id := range processes {
			if err == nil {
				err = playbook.SetActive(ctx, tx, id, false, ledger.SystemActor)
			}
		}
		return err
	})
	if page := tell(c, "Move Dev's work to Asha"); !strings.Contains(page, "Dev has 2 open items:") {
		t.Errorf("with no process on, the typed move should still give the list:\n%s", problem(page))
	}
	if _, page, _ := s.get(c, base+"/?view=tell&move_from="+s.people["dev"]+"&move_to="+s.people["asha"]); !strings.Contains(
		html.UnescapeString(page), "Dev has 2 open items:") {
		t.Errorf("with no process on, Move them… should still give the list:\n%s", page)
	}
	s.tablesMatchLedger(t)
}
