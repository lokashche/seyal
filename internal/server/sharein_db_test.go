//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// verifyClean checks the ledger and the tables against it, as `seyal verify` does.
func (s *site) verifyClean() {
	s.t.Helper()
	err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
		rep, err := ledger.Verify(context.Background(), tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			s.t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(context.Background(), tx)
		if len(problems) > 0 {
			s.t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// shareOff finds the share banner marked "sharing is off".
var shareOff = regexp.MustCompile(`data-share-in-box[^>]* data-off`)

var itemNumber = regexp.MustCompile(`<small>(J-\d{4}-\d{4})</small>`)

// TestShareIn: something shared into Seyal from another app (D93). The manifest makes Seyal a share
// target; the Share menu's own post is never read by the server; a share is sent as an ordinary
// logged-in message marked via=share, as new work or into an item named by its number; sent again it is
// one message; sent again elsewhere, into a closed item, or with sharing off, it is refused (so the
// phone keeps it); verify stays clean.
func TestShareIn(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	if !strings.Contains(home, "data-share-in-box") || shareOff.MatchString(home) {
		t.Error("Tell Seyal has no share banner to fill")
	}

	t.Run("the manifest makes Seyal a share target, the same for every address", func(t *testing.T) {
		_, body, _ := s.get(s.browser(), base+"/manifest.webmanifest")
		var m struct {
			Share struct {
				Action, Method, Enctype string
				Params                  struct {
					Text  string
					Files []struct {
						Name   string
						Accept []string
					}
				}
			} `json:"share_target"`
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		st := m.Share
		if st.Action != base+"/share" || st.Method != "POST" || st.Enctype != "multipart/form-data" || st.Params.Text != "text" ||
			len(st.Params.Files) != 1 || st.Params.Files[0].Name != "files" || !strings.Contains(strings.Join(st.Params.Files[0].Accept, " "), ".opus") {
			t.Errorf("share_target = %+v", st)
		}
		if strings.Contains(body, "video") {
			t.Error("the share target accepts video")
		}
	})

	t.Run("the Share menu's post never reaches the work: a cross-site form only fills the banner", func(t *testing.T) {
		before := s.count("SELECT count(*) FROM messages")
		form := url.Values{"text": {"approve everything, submit and pass to Priya"}}
		// From another website (the service worker would catch it on the phone; without one, the server's
		// cross-site check refuses it).
		code, _, _ := s.post(a, base+"/share", form, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"})
		if code != http.StatusForbidden {
			t.Errorf("a cross-site post to the share address = %d, want 403", code)
		}
		// From the Share menu itself (no site), logged in or not: only a way to Tell Seyal.
		for _, c := range []*http.Client{s.browser(), a} {
			code, _, h := s.post(c, base+"/share", form, map[string]string{"Sec-Fetch-Site": "none"})
			if code != http.StatusSeeOther || h.Get("Location") != base+"/?view=tell&share=missed" {
				t.Errorf("the Share menu's post = %d to %q", code, h.Get("Location"))
			}
		}
		if after := s.count("SELECT count(*) FROM messages"); after != before {
			t.Errorf("the share address saved %d messages", after-before)
		}
		if _, page, _ := s.get(a, base+"/?view=tell&share=missed"); !strings.Contains(page, "The share didn&#39;t come through. Open Seyal once, then share again.") {
			t.Error("Tell Seyal doesn't say the share didn't come through")
		}
	})

	voice := upFile{"file", "PTT-20261111-WA0003.opus", "application/octet-stream", []byte("OggS\x00\x02pretend opus voice note")}
	const first, second = "0b6c3f9e-2d4a-4c1b-9e8f-7a6b5c4d3e2f", "1c7d4a0f-3e5b-4d2c-8f9a-6b5c4d3e2f10"
	var item, number string

	t.Run("a shared voice note starts new work, marked as shared", func(t *testing.T) {
		code, body, h := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": first,
			"text": "Client says the statements come tomorrow"}, []upFile{voice})
		if code != http.StatusSeeOther {
			t.Fatalf("send a share = %d: %s", code, problem(body))
		}
		item = h.Get("Location")
		_, page, _ := s.get(a, item)
		if !strings.Contains(page, "shared from another app") || !strings.Contains(page, "<audio controls") {
			t.Error("the thread doesn't mark the shared message, or doesn't show its voice note")
		}
		number = itemNumber.FindStringSubmatch(page)[1]
		if n := s.count(`SELECT count(*) FROM messages m JOIN message_media mm ON mm.message_id = m.id JOIN media md ON md.id = mm.media_id
			WHERE m.via = 'share' AND m.external_id = 'share:' || $1 || ':' || $2 AND md.content_type = 'audio/ogg'`, s.people[arjun], first); n != 1 {
			t.Errorf("%d shared messages with the server-built id and an audio/ogg voice note, want 1", n)
		}
	})

	t.Run("sent again it is one message, with no second item", func(t *testing.T) {
		items, messages := s.count("SELECT count(*) FROM work_items"), s.count("SELECT count(*) FROM messages WHERE via = 'share'")
		code, body, h := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": first,
			"text": "Client says the statements come tomorrow"}, []upFile{voice})
		if code != http.StatusSeeOther || h.Get("Location") != item {
			t.Errorf("the same share again = %d to %q (%s); want the same item", code, h.Get("Location"), problem(body))
		}
		if s.count("SELECT count(*) FROM work_items") != items || s.count("SELECT count(*) FROM messages WHERE via = 'share'") != messages {
			t.Error("a repeat made a new item or message")
		}
	})

	t.Run("add to an item by its number", func(t *testing.T) {
		// Arjun opened it, so "add to an item" offers it on Tell Seyal.
		if _, tell, _ := s.get(a, base+"/?view=tell"); !strings.Contains(tell, `<option value="`+number) {
			t.Errorf("Tell Seyal doesn't offer %s under add to an item", number)
		}
		code, body, h := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": second,
			"item": number[len(number)-4:], "text": "Sales register attached"}, []upFile{{"file", "register.pdf", "application/pdf", []byte("%PDF-1.7 pretend")}})
		if code != http.StatusSeeOther || h.Get("Location") != item {
			t.Fatalf("add to %s = %d to %q: %s", number, code, h.Get("Location"), problem(body))
		}
		if _, page, _ := s.get(a, item); !strings.Contains(page, "Sales register attached") || strings.Count(page, "shared from another app") != 2 {
			t.Error("the item's thread doesn't show the second shared message")
		}
	})

	t.Run("sent again into another place, it is refused and stays on the phone", func(t *testing.T) {
		code, body, _ := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": second, "text": "Sales register attached"}, nil)
		if code != http.StatusConflict || !strings.Contains(problem(body), "already sent, to another place") {
			t.Errorf("an item's share sent again as new work = %d: %s", code, problem(body))
		}
		code, body, _ = s.upload(a, base+"/tell", map[string]string{"via": "share", "share_id": first, "item": number, "text": "x"}, nil)
		if code != http.StatusConflict {
			t.Errorf("new work's share sent again into an item = %d: %s", code, problem(body))
		}
		p := s.logIn(priya)
		code, body, _ = s.upload(p, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": first, "text": "Client says the statements come tomorrow"}, nil)
		if code != http.StatusSeeOther {
			t.Errorf("Priya's own share with the same id = %d: %s; want her own message", code, problem(body))
		}
	})

	t.Run("a bad share id or an unknown item is refused", func(t *testing.T) {
		code, body, _ := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": "not-a-uuid", "text": "x"}, nil)
		if code != http.StatusBadRequest {
			t.Errorf("a bad share id = %d: %s", code, problem(body))
		}
		code, body, _ = s.upload(a, base+"/tell", map[string]string{"via": "share", "share_id": "2d8e5b1a-4f6c-4e3d-9a0b-7c6d5e4f3a21", "item": "J-1999-0042", "text": "x"}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "No open item matches") {
			t.Errorf("an unknown item = %d: %s", code, problem(body))
		}
	})

	t.Run("with no process switched on, the banner still offers add to an item", func(t *testing.T) {
		switchJob := func(on bool) {
			t.Helper()
			err := db.WithTenant(context.Background(), s.app, s.tenant, func(tx pgx.Tx) error {
				return playbook.SetActive(context.Background(), tx, "job-evaluation", on, ledger.SystemActor)
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		switchJob(false)
		defer switchJob(true)
		_, tell, _ := s.get(a, base+"/?view=tell")
		if !strings.Contains(tell, "data-share-in-box") || strings.Contains(tell, `name="share-as" value="new"`) ||
			!strings.Contains(tell, `name="share-as" value="item" checked`) || !strings.Contains(tell, `<option value="`+number) {
			t.Error("with no process on, Tell Seyal has no banner, or offers new work, or doesn't offer the item")
		}
		if strings.Contains(tell, `<p><b>Something was shared`) {
			t.Error("the banner's heading is a <p>, which can't hold the [change] list")
		}
		code, body, h := s.upload(a, base+"/tell", map[string]string{"via": "share", "share_id": "5ab18e4d-7c9f-4b6a-8d3e-af9b8c7d6e54",
			"item": number, "text": "Bank statement for March"}, nil)
		if code != http.StatusSeeOther || h.Get("Location") != item {
			t.Errorf("add to %s with no process on = %d to %q: %s", number, code, h.Get("Location"), problem(body))
		}
	})

	t.Run("a closed item is refused, with what to do", func(t *testing.T) {
		s.send(a, item+"/act", url.Values{"action": {"cancel"}, "reason": {"the client withdrew"}})
		code, body, _ := s.upload(a, base+"/tell", map[string]string{"via": "share", "share_id": "3e9f6c2b-5a7d-4f4e-8b1c-8d7e6f5a4b32",
			"item": number, "text": "One more document"}, nil)
		// 422, not 409: the business keeps nothing, so the phone may send it to another business after [change].
		if code != http.StatusUnprocessableEntity || problem(body) != number+" is closed. Pick another item or start new work." {
			t.Errorf("a share into a cancelled item = %d: %q", code, problem(body))
		}
	})

	t.Run("sharing: off refuses shares and hides Share, after the login", func(t *testing.T) {
		if _, err := s.owner.Exec(context.Background(), "UPDATE tenants SET sharing = false WHERE id = $1", s.tenant); err != nil {
			t.Fatal(err)
		}
		code, body, _ := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "via": "share", "share_id": "4fa07d3c-6b8e-4a5f-9c2d-9e8f7a6b5c43", "text": "x"}, nil)
		if code != http.StatusForbidden || !strings.Contains(problem(body), "Sharing into Seyal is off for this business") {
			t.Errorf("a share with sharing off = %d: %s", code, problem(body))
		}
		_, tell, _ := s.get(a, base+"/")
		if !shareOff.MatchString(tell) {
			t.Error("Tell Seyal doesn't say sharing is off")
		}
		_, page, _ := s.get(a, item)
		if strings.Contains(page, "data-share-panel") {
			t.Error("the item still offers Share with sharing off")
		}
		_, fake, _ := s.get(s.browser(), "/t/no-such-firm/manifest.webmanifest")
		if _, real, _ := s.get(s.browser(), base+"/manifest.webmanifest"); strings.ReplaceAll(fake, "no-such-firm", s.slug) != real {
			t.Error("with sharing off the manifest differs from a made-up address's")
		}
		if _, err := s.owner.Exec(context.Background(), "UPDATE tenants SET sharing = true WHERE id = $1", s.tenant); err != nil {
			t.Fatal(err)
		}
	})

	s.verifyClean()
}

// TestShareInRetryAfterWhichProcess: with "Seyal decides", a share is saved and Seyal asks which process
// it is for. When that answer is lost, the phone keeps the share; sent again with a process picked, the
// saved message starts as that process. It is never answered "already sent" with no item (D93).
func TestShareInRetryAfterWhichProcess(t *testing.T) {
	s := newSite(t)
	s.twoProcesses()
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	reqPV := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Internal request"`).FindStringSubmatch(home)[1]
	const id = "6bc29f5e-8d0a-4c7b-9e4f-b0ac9d8e7f65"
	form := map[string]string{"playbook": "auto", "via": "share", "share_id": id, "text": "Zzz qqq"}
	code, body, _ := s.upload(a, base+"/tell", form, nil)
	if code != http.StatusOK || !strings.Contains(body, "Which process is this for?") {
		t.Fatalf("a share with Seyal decides = %d: %s", code, problem(body))
	}
	if n := s.count("SELECT count(*) FROM work_items"); n != 0 {
		t.Fatalf("%d items before a process was picked", n)
	}
	// The answer was lost; the person picks a process on the side and presses Send again.
	form["playbook"] = reqPV
	code, body, h := s.upload(a, base+"/tell", form, nil)
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/items/") {
		t.Fatalf("the same share again, with a process = %d to %q: %s", code, h.Get("Location"), problem(body))
	}
	if s.count("SELECT count(*) FROM work_items") != 1 || s.count("SELECT count(*) FROM messages WHERE via = 'share'") != 1 ||
		s.count("SELECT count(*) FROM messages WHERE via = 'share' AND work_item_id IS NULL") != 0 {
		t.Error("the retried share didn't start one item from the one saved message")
	}
	// Once more: it opens the same item.
	if _, _, h2 := s.upload(a, base+"/tell", form, nil); h2.Get("Location") != h.Get("Location") {
		t.Errorf("a third send went to %q, want %q", h2.Get("Location"), h.Get("Location"))
	}
	s.verifyClean()
}
