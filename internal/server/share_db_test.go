//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/server"
	"seyal/internal/setup"
	"seyal/internal/workitem"
)

// shareMessages is a share message for outside, put in place of the shipped Job evaluation's own for
// these tests (Phase 4, 4b-1, D91). TestShareSamples tries the shipped one (part 4b-2, D92).
const shareMessages = `
share:
  - id: ask-documents
    label: Ask for the documents
    to: outside
    in: [with_ops]
    text: >
      Dear {client}, for our work on {number} ({nature_of_work}), please send:
      {required_documents}. We need them by {client_data_due}. Fee agreed: {fee_agreed}.
      Thank you, {me}, {business}
`

// shareSite is the demo business with Asha, Ben, Dev, Chitra and Elan logged in, on a version of Job
// evaluation that has the share message above.
type shareSite struct {
	*site
	ctx      context.Context
	browsers map[string]*http.Client
}

func newShareSite(t *testing.T) *shareSite {
	t.Helper()
	ss := newShippedShareSite(t)
	ss.in(func(tx pgx.Tx) error {
		var source string
		if err := tx.QueryRow(ss.ctx, `SELECT v.source FROM tenant_playbooks t JOIN playbook_versions v
			ON v.tenant_id = t.tenant_id AND v.id = t.current_version_id
			WHERE t.tenant_id = current_tenant() AND t.playbook_id = 'job-evaluation'`).Scan(&source); err != nil {
			return err
		}
		own := strings.Index(source, "\nshare:\n")
		if !strings.Contains(source, `version: "0.7"`) || own < 0 {
			t.Fatal("the shipped Job evaluation isn't version 0.7 with share messages at the end any more; update this test")
		}
		p, err := playbook.Parse([]byte(strings.Replace(source[:own], `version: "0.7"`, `version: "0.7-share"`, 1) + shareMessages))
		if err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		id, _, err := playbook.Register(ss.ctx, tx, p)
		if err != nil {
			return err
		}
		return playbook.Use(ss.ctx, tx, id, ss.by("chitra"))
	})
	return ss
}

// newShippedShareSite is the demo business with Asha, Ben, Dev, Chitra and Elan logged in, on the
// shipped samples.
func newShippedShareSite(t *testing.T) *shareSite {
	t.Helper()
	ss := &shareSite{site: newSiteWith(t, "../../examples/playbooks"), ctx: context.Background(), browsers: map[string]*http.Client{}}
	for _, key := range []string{"asha", "ben", "dev", "chitra", "elan"} {
		var err error
		if ss.passwords[key], err = setup.NewPassword(ss.ctx, ss.app, ss.mail(key), ss.tenant); err != nil {
			t.Fatal(err)
		}
		ss.browsers[key] = ss.logIn(key)
	}
	return ss
}

func (ss *shareSite) by(key string) ledger.Actor { return ledger.PersonActor(ss.people[key]) }

func (ss *shareSite) in(fn func(tx pgx.Tx) error) {
	ss.t.Helper()
	if err := db.WithTenant(ss.ctx, ss.app, ss.tenant, fn); err != nil {
		ss.t.Fatal(err)
	}
}

// start starts a job for Sundaram Textiles as Asha and sends it to Operations, where Ben takes it.
func (ss *shareSite) start(work string) workitem.Item {
	ss.t.Helper()
	var it workitem.Item
	ss.in(func(tx pgx.Tx) error {
		var pv string
		if err := tx.QueryRow(ss.ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'job-evaluation'`).Scan(&pv); err != nil {
			return err
		}
		var err error
		if it, err = workitem.Start(ss.ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat", By: ss.by("asha"),
			Fields: engine.Fields{"client": "sundaram-textiles", "nature_of_work": work, "reason": "Accounts for the year",
				"integrity_check": "pass"}}); err != nil {
			return err
		}
		if _, err = workitem.Act(ss.ctx, tx, it.ID, "send_to_ops", "", ss.by("asha")); err != nil {
			return err
		}
		it, err = workitem.Take(ss.ctx, tx, it.ID, "team:ops", "button", ss.by("ben"))
		return err
	})
	return it
}

// shares are the messages the person's Share panel offers on the item.
func (ss *shareSite) shares(key, id string) []workitem.ShareText {
	ss.t.Helper()
	var list []workitem.ShareText
	ss.in(func(tx pgx.Tx) error {
		it, err := workitem.Get(ss.ctx, tx, id)
		if err != nil {
			return err
		}
		list, err = workitem.Shares(ss.ctx, tx, it, ss.by(key), workitem.ShareEnv{
			Link: "https://seyal.example/t/" + ss.slug + "/items/" + id, Business: "Kaveri Associates (demo)", Currency: "INR"})
		return err
	})
	return list
}

// offered is one message as the person's item page offers it: the ready words and the signed offer.
type offered struct{ Text, Offer string }

var offeredRadio = regexp.MustCompile(`name="share-msg" value="([^"]+)"[^>]*data-text="([^"]*)" data-offer="([^"]+)"`)

// offers reads the Share panel's messages from the person's item page, by message id.
func (ss *shareSite) offers(key, id string) map[string]offered {
	ss.t.Helper()
	_, page, _ := ss.get(ss.browsers[key], "/t/"+ss.slug+"/items/"+id)
	out := map[string]offered{}
	for _, m := range offeredRadio.FindAllStringSubmatch(page, -1) {
		out[m[1]] = offered{Text: html.UnescapeString(m[2]), Offer: m[3]}
	}
	return out
}

// report posts a hand-over as the person's page does, and returns the answer's code and JSON.
func (ss *shareSite) report(key, id string, form url.Values) (int, map[string]any) {
	ss.t.Helper()
	code, body, _ := ss.post(ss.browsers[key], "/t/"+ss.slug+"/items/"+id+"/shared", form, nil)
	var j map[string]any
	json.Unmarshal([]byte(body), &j)
	return code, j
}

// sharedEvents are the item's item.shared events: each one's change, actor and snapshot.
func (ss *shareSite) sharedEvents(id string) []struct {
	Change map[string]any
	Actor  string
	Item   workitem.Item
} {
	ss.t.Helper()
	var out []struct {
		Change map[string]any
		Actor  string
		Item   workitem.Item
	}
	ss.in(func(tx pgx.Tx) error {
		rows, _ := tx.Query(ss.ctx, `SELECT body, actor_kind || ':' || coalesce(actor_person_id::text, '') FROM ledger
			WHERE subject_id = $1::uuid AND kind = 'item.shared' ORDER BY seq`, id)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Body, Actor string }])
		for _, l := range list {
			var e struct {
				Payload struct {
					Change map[string]any `json:"change"`
					Item   workitem.Item  `json:"item"`
				} `json:"payload"`
			}
			if err := json.Unmarshal([]byte(l.Body), &e); err != nil {
				return err
			}
			out = append(out, struct {
				Change map[string]any
				Actor  string
				Item   workitem.Item
			}{e.Payload.Change, l.Actor, e.Payload.Item})
		}
		return err
	})
	return out
}

// row is what a share must not change on the item's own row.
func (ss *shareSite) row(id string) string {
	ss.t.Helper()
	var r string
	ss.in(func(tx pgx.Tx) error {
		return tx.QueryRow(ss.ctx, `SELECT concat_ws('|', version, fields_sha256, updated_at, held_since, step_since,
			assignee_id, team, state) FROM work_items WHERE id = $1::uuid`, id).Scan(&r)
	})
	return r
}

const (
	attempt1 = "6f1c2a40-1b2c-4d3e-8f90-0a1b2c3d4e5f"
	attempt2 = "7a2d3b51-2c3d-4e4f-9a01-1b2c3d4e5f60"
	attempt3 = "8b3e4c62-3d4e-4f50-8b12-2c3d4e5f6071"
	attempt4 = "9c4f5d73-4e5f-4061-9c23-3d4e5f607182"
	attempt5 = "ad506e84-5f60-4172-8d34-4e5f60718293"
	attempt6 = "be617f95-6071-4283-9e45-5f6071829304"
)

// TestShareOut follows part 4b-1 (D91) with the demo people: the Share panel's messages and their
// words, an outside message refused for Elan by the server, a double tap recorded once, the thread's
// honest lines, and a share that changes nothing on the item: no new version, approvals stay, the New
// mark and the briefing don't react, and the ledger and tables still agree.
func TestShareOut(t *testing.T) {
	ss := newShareSite(t)
	base := "/t/" + ss.slug
	job := ss.start("annual-accounts")
	page := base + "/items/" + job.ID
	ss.in(func(tx pgx.Tx) error {
		_, err := workitem.SetFields(ss.ctx, tx, job.ID, engine.Fields{"fee_agreed": 1500000.0}, ss.by("ben"))
		return err
	})

	// Ben (who has it) is offered both messages; Elan (Owner, may not change it) only Tell a colleague.
	ben := ss.shares("ben", job.ID)
	if len(ben) != 2 || ben[0].ID != playbook.BuiltinShare || ben[0].Label != "Tell a colleague" || ben[1].ID != "ask-documents" {
		t.Fatalf("Ben's messages = %+v; want Tell a colleague, then Ask for the documents", ben)
	}
	colleague, outside := ben[0].Text, ben[1].Text
	link := "https://seyal.example/t/" + ss.slug + "/items/" + job.ID
	if !strings.HasPrefix(colleague, job.Number+" · Sundaram Textiles — Annual accounts · with ops.") ||
		!strings.HasSuffix(colleague, "\nOpen in Seyal: "+link) {
		t.Errorf("Tell a colleague = %q", colleague)
	}
	for _, want := range []string{"Dear Sundaram Textiles, for our work on " + job.Number + " (Annual accounts), please send: Trial balance, Bank statements",
		"We need them by [Client data due: empty].", "Fee agreed: ₹15,000.00.", "Thank you, Ben, Kaveri Associates (demo)"} {
		if !strings.Contains(outside, want) {
			t.Errorf("Ask for the documents = %q; want it to contain %q", outside, want)
		}
	}
	if strings.Contains(outside, "/items/") || !slices.Equal(ben[1].Empty, []string{"Client data due"}) {
		t.Errorf("Ask for the documents carries a link or misses the empty detail: %q, empty %v", outside, ben[1].Empty)
	}
	if elan := ss.shares("elan", job.ID); len(elan) != 1 || elan[0].ID != playbook.BuiltinShare {
		t.Errorf("Elan's messages = %+v; want only Tell a colleague", elan)
	}
	_, benPage, _ := ss.get(ss.browsers["ben"], page)
	_, elanPage, _ := ss.get(ss.browsers["elan"], page)
	if !strings.Contains(benPage, "data-share-panel") || !strings.Contains(benPage, "Ask for the documents") {
		t.Error("Ben's item page has no Share panel with Ask for the documents")
	}
	if !strings.Contains(elanPage, "data-share-panel") || strings.Contains(elanPage, "Ask for the documents") {
		t.Error("Elan's item page: want the Share panel without the outside message")
	}

	// Elan reports the outside message anyway (a script, an old page): the server refuses it.
	form := func(share, route, text, attempt string) url.Values {
		return url.Values{"share": {share}, "route": {route}, "text": {text}, "attempt": {attempt}}
	}
	if code, j := ss.report("elan", job.ID, form("ask-documents", "menu", outside, attempt1)); code != http.StatusForbidden || j["error"] == nil {
		t.Errorf("Elan's outside share = %d %v; want 403 with a reason", code, j)
	}
	// Records that can't be right.
	for name, f := range map[string]url.Values{
		"an unknown message": form("no-such-message", "copy", "Hello", attempt1),
		"an unknown way":     form(playbook.BuiltinShare, "fax", "Hello", attempt1),
		"a bad attempt id":   form(playbook.BuiltinShare, "copy", "Hello", "not-a-uuid"),
		"an empty text":      form(playbook.BuiltinShare, "copy", "  ", attempt1),
		"a text over 4000":   form(playbook.BuiltinShare, "copy", strings.Repeat("அ", 4001), attempt1),
	} {
		want := http.StatusBadRequest
		if name == "an unknown message" {
			want = http.StatusForbidden
		}
		if code, _ := ss.report("ben", job.ID, f); code != want {
			t.Errorf("%s = %d; want %d", name, code, want)
		}
	}
	if got := ss.sharedEvents(job.ID); len(got) != 0 {
		t.Fatalf("refused shares recorded %d events", len(got))
	}

	// Ben's page offers both messages, each with its signed offer. Elan sends Ben's offer as his own: it
	// counts only for Ben, so the server still refuses it.
	benOffers := ss.offers("ben", job.ID)
	asked, ok := benOffers["ask-documents"]
	if !ok || benOffers[playbook.BuiltinShare].Offer == "" || !strings.Contains(asked.Text, "Thank you, Ben") {
		t.Fatalf("Ben's page offers %+v; want both messages with signed offers", benOffers)
	}
	withOffer := func(f url.Values, offer string) url.Values { f.Set("offer", offer); return f }
	if code, _ := ss.report("elan", job.ID, withOffer(form("ask-documents", "menu", asked.Text, attempt1), asked.Offer)); code != http.StatusForbidden {
		t.Errorf("Elan's outside share with Ben's offer = %d; want 403", code)
	}

	// Ben edits the words and copies them; his page reports it twice (a double tap): one event.
	before := ss.row(job.ID)
	edited := asked.Text + " Please call me."
	for i, want := range []bool{true, false} {
		code, j := ss.report("ben", job.ID, withOffer(form("ask-documents", "copy", edited, attempt2), asked.Offer))
		if code != http.StatusOK || j["ok"] != true || j["recorded"] != want {
			t.Errorf("Ben's copy, report %d = %d %v; want ok, recorded %v", i+1, code, j, want)
		}
	}
	events := ss.sharedEvents(job.ID)
	if len(events) != 1 {
		t.Fatalf("%d item.shared events after a double tap, want 1", len(events))
	}
	e := events[0]
	if e.Change["share"] != "ask-documents" || e.Change["to"] != "outside" || e.Change["route"] != "copy" ||
		e.Change["text"] != edited || e.Change["edited"] != true || e.Change["attempt"] != attempt2 || e.Actor != "human:"+ss.people["ben"] ||
		e.Change["offered_version"] != float64(job.Version+1) {
		t.Errorf("the event = %v by %s", e.Change, e.Actor)
	}
	if e.Item.Version != job.Version+1 || e.Item.ID != job.ID { // +1: the fee Ben set
		t.Errorf("the event's snapshot is version %d of %s; want the item as it was", e.Item.Version, e.Item.ID)
	}
	if after := ss.row(job.ID); after != before {
		t.Errorf("a share changed the item's row:\n before %s\n after  %s", before, after)
	}

	// The thread: honest words, the app named only by the page, the exact words behind the line.
	_, benPage, _ = ss.get(ss.browsers["ben"], page)
	if !strings.Contains(benPage, "Ben copied ‘Ask for the documents’") || !strings.Contains(benPage, "Please call me.") ||
		!strings.Contains(benPage, "with their own changes to Seyal’s words") {
		t.Error("the thread doesn't show Ben's copy with its words")
	}
	if code, _ := ss.report("asha", job.ID, form(playbook.BuiltinShare, "chat_link", colleague, attempt3)); code != http.StatusOK {
		t.Fatalf("Asha's chat link share = %d", code)
	}
	_, ashaPage, _ := ss.get(ss.browsers["asha"], page)
	if !strings.Contains(ashaPage, "Asha opened a WhatsApp link with ‘Tell a colleague’") ||
		!strings.Contains(ashaPage, "Seyal can’t see whether it was sent.") {
		t.Error("the thread doesn't name the chat app for Asha's chat link, or claims it was sent")
	}

	// Ben submits and approves for Operations. Dev opened the item; Ben's message is no longer offered
	// in this step. Dev shares Tell a colleague: no new version, Ben's approval stays, Dev's card isn't
	// New, the briefings don't change.
	ss.in(func(tx pgx.Tx) error {
		if _, err := workitem.Act(ss.ctx, tx, job.ID, "submit", "", ss.by("ben")); err != nil {
			return err
		}
		_, err := workitem.Decide(ss.ctx, tx, job.ID, ss.people["ben"], "ops", engine.Approve, "")
		return err
	})
	if code, _ := ss.report("ben", job.ID, form("ask-documents", "copy", outside, attempt4)); code != http.StatusForbidden {
		t.Errorf("Ask for the documents while approvals wait = %d; want 403 (offered only with Operations)", code)
	}
	// But Ben's page was drawn while the item was with Operations, and he hands the words on from it
	// unchanged (a page left open, or a record that waited on an offline phone): it is judged by what
	// Seyal offered then, recorded once, and not marked as changed although the item has moved on.
	for i, want := range []bool{true, false} {
		code, j := ss.report("ben", job.ID, withOffer(form("ask-documents", "menu", asked.Text, attempt5), asked.Offer))
		if code != http.StatusOK || j["recorded"] != want {
			t.Errorf("Ben's late record, report %d = %d %v; want ok, recorded %v", i+1, code, j, want)
		}
	}
	if events = ss.sharedEvents(job.ID); len(events) != 3 {
		t.Fatalf("%d item.shared events after Ben's late record; want 3", len(events))
	} else if c := events[2].Change; c["edited"] != false || c["offered_version"] != float64(job.Version+1) || c["to"] != "outside" {
		t.Errorf("Ben's late record = %v; want unchanged words offered at version %d", c, job.Version+1)
	}
	ss.get(ss.browsers["dev"], page)
	brief := regexp.MustCompile(`(?s)<p class="brief">.*?</p>`)
	briefs := map[string]string{}
	for _, key := range []string{"asha", "dev", "chitra"} {
		_, home, _ := ss.get(ss.browsers[key], base+"/")
		briefs[key] = brief.FindString(home)
	}
	before = ss.row(job.ID)
	if code, j := ss.report("dev", job.ID, form(playbook.BuiltinShare, "menu", colleague, attempt1)); code != http.StatusOK || j["recorded"] != true {
		t.Fatalf("Dev's share = %d %v", code, j)
	}
	// Dev's record came with no offer (as after a server restart): it can only say whether the words
	// differ from Seyal's words now, never that Dev changed them.
	if c := ss.sharedEvents(job.ID)[3].Change; c["edited"] != nil || c["differs"] == nil || c["offered_version"] != nil {
		t.Errorf("a record without an offer = %v; want \"differs\" only", c)
	}
	if after := ss.row(job.ID); after != before {
		t.Errorf("a share changed the item's row:\n before %s\n after  %s", before, after)
	}
	ss.in(func(tx pgx.Tx) error {
		st, err := workitem.Approvals(ss.ctx, tx, job.ID)
		if err != nil {
			return err
		}
		for _, sl := range st.Slots {
			if sl.Slot == "ops" && sl.ApprovedBy != ss.people["ben"] {
				t.Error("Ben's approval for Operations is gone after a share")
			}
		}
		it, err := workitem.Get(ss.ctx, tx, job.ID)
		if err != nil {
			return err
		}
		seen, err := workitem.SeenBy(ss.ctx, tx, ss.people["dev"])
		if workitem.IsNew(it, ss.people["dev"], seen[job.ID]) {
			t.Error("Dev's card is New after a share")
		}
		return err
	})
	for key, want := range briefs {
		if _, home, _ := ss.get(ss.browsers[key], base+"/"); brief.FindString(home) != want {
			t.Errorf("%s's briefing changed after a share:\n before %s\n after  %s", key, want, brief.FindString(home))
		}
	}

	// A closed item: Tell a colleague only, and the outside message is refused.
	other := ss.start("monthly-books")
	ss.in(func(tx pgx.Tx) error {
		_, err := workitem.Act(ss.ctx, tx, other.ID, "cancel", "The client chose another firm", ss.by("ben"))
		return err
	})
	if list := ss.shares("ben", other.ID); len(list) != 1 || list[0].ID != playbook.BuiltinShare {
		t.Errorf("a cancelled item offers %+v; want only Tell a colleague", list)
	}
	if code, _ := ss.report("ben", other.ID, form("ask-documents", "copy", "Hello", attempt1)); code != http.StatusForbidden {
		t.Errorf("an outside share on a cancelled item = %d; want 403", code)
	}

	// Each person may record at most 30 shares a minute (Elan has used 2).
	code := 0
	for range 28 {
		code, _ = ss.report("elan", job.ID, form(playbook.BuiltinShare, "fax", "Hello", attempt1))
	}
	if code != http.StatusBadRequest {
		t.Errorf("Elan's 30th report = %d; want it read (400 for its bad way)", code)
	}
	if code, _ = ss.report("elan", job.ID, form(playbook.BuiltinShare, "copy", colleague, attempt3)); code != http.StatusTooManyRequests {
		t.Errorf("Elan's 31st report in a minute = %d; want 429", code)
	}

	ss.tablesMatchLedger(t)
}

// TestShareKeepsEscalatedWork: a share in an escalated step stay (D75, decision 11) carries the item as
// it was, so the approval of whoever worked the step after it was escalated still says so.
func TestShareKeepsEscalatedWork(t *testing.T) {
	ss := newShareSite(t)
	job := ss.start("annual-accounts")
	zone, _ := time.LoadLocation("Asia/Kolkata")
	chase := func(do func(context.Context, pgx.Tx, string, workitem.Chase) (string, error), days int) {
		t.Helper()
		ss.in(func(tx pgx.Tx) error {
			it, err := workitem.Get(ss.ctx, tx, job.ID)
			if err != nil {
				return err
			}
			did, err := do(ss.ctx, tx, job.ID, workitem.Chase{StepSince: it.StepSince, Reminders: it.Reminders, Days: days,
				Now: time.Now(), Zone: zone})
			if did == "" && err == nil {
				t.Errorf("nothing was done on day %d", days)
			}
			return err
		})
	}
	chase(workitem.Remind, 2)
	chase(workitem.Escalate, 4)
	// Elan shares straight after the escalation; Chitra shares too, then takes it and does the work.
	for key, attempt := range map[string]string{"elan": attempt1, "chitra": attempt2} {
		if code, j := ss.report(key, job.ID, url.Values{"share": {playbook.BuiltinShare}, "route": {"copy"},
			"text": {"Please look at " + job.Number}, "attempt": {attempt}}); code != http.StatusOK || j["recorded"] != true {
			t.Fatalf("%s's share = %d %v", key, code, j)
		}
	}
	var worked []string
	ss.in(func(tx pgx.Tx) error {
		if _, err := workitem.Take(ss.ctx, tx, job.ID, "team:owner", "button", ss.by("chitra")); err != nil {
			return err
		}
		if _, err := workitem.SetFields(ss.ctx, tx, job.ID, engine.Fields{"fee_agreed": 3500000.0}, ss.by("chitra")); err != nil {
			return err
		}
		var err error
		worked, err = workitem.WorkedEscalated(ss.ctx, tx, job.ID, ss.people["chitra"])
		return err
	})
	if !slices.Equal(worked, []string{"with_ops"}) {
		t.Errorf("Chitra worked %v after the escalation; want [with_ops]", worked)
	}
	ss.tablesMatchLedger(t)
}

// TestSharedLinkNeedsLogin: the link in Tell a colleague opens nothing without a login. A link
// preview (an app fetching it with no login) gets only the login page: titled "Log in · Seyal", with no preview
// tags and no business name (D29; Phase 4 design, decision 9).
func TestSharedLinkNeedsLogin(t *testing.T) {
	ss := newShareSite(t)
	job := ss.start("annual-accounts")
	link := "/t/" + ss.slug + "/items/" + job.ID
	code, body, h := ss.get(ss.browser(), link)
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/t/"+ss.slug+"/login") {
		t.Fatalf("the item's link without a login = %d to %q", code, h.Get("Location"))
	}
	if strings.Contains(body, job.Number) || strings.Contains(body, "Sundaram") {
		t.Error("the redirect shows the item")
	}
	_, login, _ := ss.get(ss.browser(), h.Get("Location"))
	if !strings.Contains(login, "<title>Log in · Seyal</title>") {
		t.Error("the login page's title isn't the plain \"Log in · Seyal\"")
	}
	for _, leak := range []string{"og:", "twitter:", `property="`, `name="description"`, "Kaveri", "Sundaram", job.Number} {
		if strings.Contains(login, leak) {
			t.Errorf("the login page a link preview gets contains %q", leak)
		}
	}
}

// TestShareSamples follows part 4b-2 (D92) on the shipped Job evaluation 0.7: Ben (who has the job
// with Operations) is offered "Ask the client for documents", filled in from the job, with the empty
// detail marked; Sundaram Textiles' phone (as digits) and email come from the client list's contact:;
// the WhatsApp button shows only with SEYAL_SHARE_CHAT_LINKS on; an email that would add receivers
// ("?bcc=") is never offered; Settings → Processes says it in plain words.
func TestShareSamples(t *testing.T) {
	ss := newShippedShareSite(t)
	job := ss.start("annual-accounts")
	page := "/t/" + ss.slug + "/items/" + job.ID

	ben := ss.shares("ben", job.ID)
	if len(ben) != 2 || ben[1].ID != "ask-documents" || ben[1].Label != "Ask the client for documents" || ben[1].To != "outside" {
		t.Fatalf("Ben's messages = %+v; want Tell a colleague, then Ask the client for documents", ben)
	}
	ask := ben[1]
	want := "Dear Sundaram Textiles, for our work on " + job.Number + " (Annual accounts), please send: Trial balance, " +
		"Bank statements, Fixed asset register, Loan statements. We need them by [Client data due: empty]. " +
		"Thank you, Ben, Kaveri Associates (demo)"
	if ask.Text != want || !slices.Equal(ask.Empty, []string{"Client data due"}) {
		t.Errorf("Ask the client for documents = %q, empty %v\nwant %q", ask.Text, ask.Empty, want)
	}
	if ask.Phone != "910000000001" || ask.Email != "accounts@sundaram.example" {
		t.Errorf("the contact = %q, %q; want Sundaram's phone as digits and email", ask.Phone, ask.Email)
	}
	if ben[0].Phone != "" || ben[0].Email != "" {
		t.Errorf("Tell a colleague carries a contact: %+v", ben[0])
	}

	// The date filled in: no empty detail left, the date with the month's name.
	ss.in(func(tx pgx.Tx) error {
		_, err := workitem.SetFields(ss.ctx, tx, job.ID, engine.Fields{"client_data_due": "2026-11-12"}, ss.by("ben"))
		return err
	})
	if ask = ss.shares("ben", job.ID)[1]; !strings.Contains(ask.Text, "We need them by 12 Nov 2026.") || len(ask.Empty) != 0 {
		t.Errorf("with the date filled: %q, empty %v", ask.Text, ask.Empty)
	}

	// The page: the contact's email on the message, and no WhatsApp button while the switch is off.
	// Nor the phone: only the WhatsApp button uses it, so with the switch off it stays out of the page.
	_, benPage, _ := ss.get(ss.browsers["ben"], page)
	if !strings.Contains(benPage, `data-email="accounts@sundaram.example"`) {
		t.Error("Ben's page doesn't carry Sundaram's email on the message")
	}
	if strings.Contains(benPage, "data-phone") || strings.Contains(benPage, "910000000001") {
		t.Error("Sundaram's phone is in the page with SEYAL_SHARE_CHAT_LINKS off")
	}
	if strings.Contains(benPage, `data-share-route="chat_link"`) || strings.Contains(benPage, "data-chat-links") ||
		strings.Contains(benPage, "data-share-chat-web") {
		t.Error("the WhatsApp button shows with SEYAL_SHARE_CHAT_LINKS off")
	}
	srv, err := server.New(server.Config{App: ss.app, Store: ss.store, Library: ss.lib, AI: ss.ai, ChatLinks: true})
	if err != nil {
		t.Fatal(err)
	}
	on := httptest.NewServer(srv.Handler())
	defer on.Close()
	resp, err := ss.browsers["ben"].Get(on.URL + page)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `data-share-route="chat_link">WhatsApp</button>`) || !strings.Contains(string(body), "data-chat-links") ||
		!strings.Contains(string(body), "WhatsApp may open whatsapp.com, which sees the text.") ||
		!strings.Contains(string(body), `data-phone="910000000001" data-email="accounts@sundaram.example"`) ||
		!strings.Contains(string(body), "data-share-chat-web hidden") {
		t.Errorf("with SEYAL_SHARE_CHAT_LINKS on (%d), the page has no WhatsApp button, note, contact and \"didn't open?\" link", resp.StatusCode)
	}

	// Someone writes an email that would add a hidden receiver: the Email button gets no address.
	ss.in(func(tx pgx.Tx) error {
		kind, err := playbook.Kind(ss.ctx, tx, "client")
		if err != nil {
			return err
		}
		it, err := master.Get(ss.ctx, tx, "client", "sundaram-textiles")
		if err != nil {
			return err
		}
		it.Fields["email"] = "accounts@sundaram.example?bcc=someone@elsewhere.example"
		_, _, err = master.Save(ss.ctx, tx, kind, it.Key, it.Name, it.Fields, ss.by("chitra"))
		return err
	})
	if ask = ss.shares("ben", job.ID)[1]; ask.Email != "" || ask.Phone != "910000000001" {
		t.Errorf("with a ?bcc= email: phone %q, email %q; want the phone and no email", ask.Phone, ask.Email)
	}
	if _, benPage, _ = ss.get(ss.browsers["ben"], page); strings.Contains(benPage, "data-email=") || strings.Contains(benPage, "bcc") {
		t.Error("Ben's page offers the ?bcc= email")
	}

	// Settings → Processes says it in plain words.
	_, settings, _ := ss.get(ss.browsers["chitra"], "/t/"+ss.slug+"/settings/processes/job-evaluation")
	if !strings.Contains(html.UnescapeString(settings), "Ask the client for documents (for outside; in studying, with ops; phone and email from Client)") {
		t.Error("Settings → Processes doesn't describe the share message in plain words")
	}
	ss.tablesMatchLedger(t)
}
