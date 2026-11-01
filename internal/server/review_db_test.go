//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// TestReviewFixes covers what the review pass found in the web layer.
func TestReviewFixes(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]

	t.Run("a file whose type hides a web page is refused", func(t *testing.T) {
		trick := upFile{"file", "x.png", "image/png,text/html", []byte("<script>alert(1)</script>")}
		code, body, _ := s.upload(a, base+"/tell", map[string]string{"playbook": pv, "text": "see file"}, []upFile{trick})
		if code == http.StatusSeeOther || !strings.Contains(problem(body), "not accepted") {
			t.Errorf("upload with a disguised type = %d: %s", code, problem(body))
		}
	})

	t.Run("a command wins over Seyal's open question", func(t *testing.T) {
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"Work for someone new"}}, nil)
		item := h.Get("Location")
		page := html.UnescapeString(s.send(a, item+"/say", url.Values{"text": {"cancel because it was a test"}}))
		if !strings.Contains(page, "cancelled") || strings.Contains(page, "as a new") {
			t.Errorf("cancel while Seyal asks for the client should cancel, not offer a new client:\n%s", page)
		}
	})

	t.Run("only the holder, the starter or an admin changes an item; anyone may write", func(t *testing.T) {
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"Books for Ganga"}}, nil)
		item := h.Get("Location") // Arjun started it and has it
		p := s.logIn(priya)       // ops: neither holder nor starter nor admin
		_, page, _ := s.get(p, item)
		if !strings.Contains(page, "or an admin can change this item") || strings.Contains(page, `name="action" value="cancel"`) ||
			strings.Contains(page, `aria-label="Edit `) {
			t.Errorf("Priya should see the item read-only, without actions or edit links:\n%s", problem(page))
		}
		code, body, _ := s.post(p, item+"/act", url.Values{"action": {"cancel"}, "reason": {"not mine"}}, nil)
		if code == http.StatusSeeOther || !strings.Contains(problem(body), "Only whoever has this item") {
			t.Errorf("Priya cancelling Arjun's item = %d: %s", code, problem(body))
		}
		if code, _, _ := s.post(p, item+"/say", url.Values{"text": {"FYI: their documents arrived"}}, nil); code != http.StatusSeeOther {
			t.Errorf("Priya writing a note = %d, want it accepted", code)
		}
		if _, page, _ = s.get(s.logIn(lakshmi), item); !strings.Contains(page, `name="action" value="cancel"`) {
			t.Error("Lakshmi (admin) should be able to act on it")
		}
	})

	t.Run("a new password ends the person's other sessions", func(t *testing.T) {
		l := s.logIn(lakshmi)
		code, _, _ := s.post(l, base+"/settings/people/"+s.people["arjun"]+"/password", nil, nil)
		if code != http.StatusOK {
			t.Fatalf("new password = %d", code)
		}
		if code, _, h := s.get(a, base+"/"); code != http.StatusSeeOther || !strings.HasSuffix(h.Get("Location"), "/login") {
			t.Errorf("Arjun's old session after a new password = %d to %q, want the login page", code, h.Get("Location"))
		}
	})
}
