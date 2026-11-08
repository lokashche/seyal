// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"seyal/internal/auth"
	"seyal/internal/seal"
	"seyal/internal/workitem"
	"seyal/web"
)

// TestOpenOffer: a hand-over is judged by the offer signed into the page only when the offer is this
// server's, for the same business and person, and not too old (D91). Anything else is read as no offer.
func TestOpenOffer(t *testing.T) {
	s := &Server{shareKey: seal.NewKey()}
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &call{tenant: tenant{ID: "tenant-a"}, session: auth.Session{PersonID: "person-a"}}
	offer := workitem.ShareOffer{Item: "item-1", Share: "ask-documents", To: "outside", Version: 3,
		Digest: workitem.ShareDigest("Dear Acme Traders")}
	sign := func(key []byte, n offerNote) string {
		tok, err := seal.Sign(key, n)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	good := offerNote{Tenant: "tenant-a", Person: "person-a", At: now.Add(-time.Hour).Unix(), ShareOffer: offer}
	if got := s.openOffer(c, sign(s.shareKey, good), now); got == nil || *got != offer {
		t.Fatalf("a good offer read as %+v", got)
	}
	other := func(change func(*offerNote)) offerNote { n := good; change(&n); return n }
	for name, tok := range map[string]string{
		"none":                "",
		"another server's":    sign(seal.NewKey(), good),
		"changed in the page": sign(s.shareKey, good) + "x",
		"another business's":  sign(s.shareKey, other(func(n *offerNote) { n.Tenant = "tenant-b" })),
		"another person's":    sign(s.shareKey, other(func(n *offerNote) { n.Person = "person-b" })),
		"over 7 days old":     sign(s.shareKey, other(func(n *offerNote) { n.At = now.Add(-offerLife - time.Minute).Unix() })),
		"from the far future": sign(s.shareKey, other(func(n *offerNote) { n.At = now.Add(time.Hour).Unix() })),
	} {
		if got := s.openOffer(c, tok, now); got != nil {
			t.Errorf("an offer %s was counted: %+v", name, got)
		}
	}
}

// TestSharePageScript: the page's script and styles keep the Share panel's promises (D91): a record
// carries its signed offer, waits under the login (not the session) and only for its own business,
// nothing over 4,000 letters leaves, a hidden button stays hidden, and a log out sends what waits first.
func TestSharePageScript(t *testing.T) {
	js, err := fs.ReadFile(web.Files, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`offer: r.offer || ""`, `getAttribute("data-offer")`, `r.sharer === sharer`,
		`r.url.indexOf(here) === 0`, `letters(text) > SHARE_MAX`, `panel.edits[m.value] = e.target.value`,
		`waiting ? sendShares() : null`, `Date.now() - r.at < SHARE_LIFE`,
		// D92: the WhatsApp and Email links come from links.js, counted after encoding; WhatsApp only when
		// the server switched it on, the app's own address first on a phone.
		`SeyalLinks.buttons(`, `panel.hasAttribute("data-chat-links")`, `location.href = c.app`, `case "chat_link":`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js no longer has %q", want)
		}
	}
	css, err := fs.ReadFile(web.Files, "static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".share [hidden], [data-share][hidden] { display: none; }") {
		t.Error("app.css no longer keeps hidden share buttons hidden (.btn's display beats [hidden])")
	}
	layout, err := fs.ReadFile(web.Files, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(layout), `data-sharer="{{.Session.Tag}}"`) {
		t.Error("layout.html no longer marks the page with the login's tag")
	}
	// links.js runs before app.js, and the service worker keeps it for the offline start.
	if a, b := strings.Index(string(layout), "/static/links.js"), strings.Index(string(layout), "/static/app.js"); a < 0 || a > b {
		t.Error("layout.html doesn't load links.js before app.js")
	}
	sw, err := fs.ReadFile(web.Files, "templates/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sw), `"/static/links.js?v=" + VERSION`) {
		t.Error("sw.js doesn't keep links.js")
	}
}
