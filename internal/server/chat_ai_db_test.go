//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"seyal/internal/ai"
)

// TestOfferShowsYesNo: when Seyal offers to add a new entry, the quick answers are yes and no, not the
// list's entries; after "yes" the entry is in and Seyal moves on.
func TestOfferShowsYesNo(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	s.ai.Set(ai.Understand, ai.Choice{Name: "fake:test", Model: &ai.Fake{Answer: func(ai.Request) (string, error) {
		return `{"kind":"new_work","fields":{"client":"Rasi Textiles"},"new_items":[{"field":"client","name":"Rasi Textiles"}],
			"command":"","reason":"","reply":""}`, nil
	}}})
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)

	code, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv[1]}, "text": {"Work for Rasi Textiles"}}, nil)
	if code != http.StatusSeeOther {
		t.Fatalf("tell = %d", code)
	}
	item := h.Get("Location")
	_, page, _ := s.get(a, item)
	if !strings.Contains(page, "Add it as a new") || !strings.Contains(page, ">yes</button>") ||
		!strings.Contains(page, ">no</button>") || strings.Contains(page, ">Sundaram Textiles</button>") {
		t.Fatalf("the offer doesn't show yes / no as quick answers:\n%s", page)
	}

	// "yes" answers the offer directly; it doesn't go to the model.
	if code, body, _ := s.post(a, item+"/say", url.Values{"text": {"yes"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("say yes = %d: %s", code, problem(body))
	}
	if _, page, _ = s.get(a, item); !strings.Contains(page, "Added Rasi Textiles.") || strings.Contains(page, ">yes</button>") {
		t.Errorf("after yes, the page should say it was added and stop offering:\n%s", page)
	}
}
