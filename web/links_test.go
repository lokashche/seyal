// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// linksScript runs static/links.js in Node with the cases below and prints, for each, the links the
// Share panel would offer (D92). It runs the page's own code, so the test checks what people get.
const linksScript = `
const L = require(process.argv[1]);
const cases = JSON.parse(process.argv[2]);
console.log(JSON.stringify(cases.map((c) => L.buttons(c))));
`

type linkCase struct {
	ChatLinks bool   `json:"chatLinks"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	Subject   string `json:"subject"`
	Text      string `json:"text"`
}

type linkResult struct {
	Chat *struct{ App, Web string } `json:"chat"`
	Mail string                     `json:"mail"`
}

// TestShareLinks: the WhatsApp and Email links are counted after encoding, so a long Tamil text (each
// letter takes 9 characters in a link) hides both buttons although it is far under 4,000 letters; a
// short one goes to the contact's number and address; the WhatsApp link exists only when switched on.
// It needs Node; without it the test is skipped (the script was then not run).
func TestShareLinks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, so static/links.js can't be run here")
	}
	script, err := filepath.Abs("static/links.js")
	if err != nil {
		t.Fatal(err)
	}
	tamil := strings.Repeat("ஆவணங்களை அனுப்பவும். ", 25) // "please send the documents", about 500 letters
	short := "Dear Sundaram Textiles, please send: bank statements & sales register. Fee: ₹15,000.00"
	cases := []linkCase{
		{ChatLinks: true, Phone: "910000000001", Email: "accounts@sundaram.example", Subject: "Ask · J-2026-0007", Text: short},
		{ChatLinks: true, Phone: "910000000001", Email: "accounts@sundaram.example", Subject: "Ask · J-2026-0007", Text: tamil},
		{ChatLinks: false, Subject: "Ask · J-2026-0007", Text: short},
		{ChatLinks: true, Subject: "Ask · J-2026-0007", Text: short},
	}
	in, _ := json.Marshal(cases)
	out, err := exec.Command(node, "-e", linksScript, script, string(in)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []linkResult
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node printed %s (%v)", out, err)
	}

	// A short text: both links, to the contact, every part encoded (a space is %20, "&" can't start a
	// new part of the link).
	if c := got[0].Chat; c == nil || !strings.HasPrefix(c.Web, "https://wa.me/910000000001?text=Dear%20Sundaram") ||
		!strings.HasPrefix(c.App, "whatsapp://send?phone=910000000001&text=Dear") || strings.Contains(c.Web, " & ") ||
		!strings.Contains(c.Web, "%26%20sales") || !strings.Contains(c.Web, "%E2%82%B915%2C000.00") {
		t.Errorf("the WhatsApp link for a short text = %+v", c)
	}
	if m := got[0].Mail; !strings.HasPrefix(m, "mailto:accounts@sundaram.example?subject=Ask%20%C2%B7%20J-2026-0007&body=Dear%20") {
		t.Errorf("the Email link for a short text = %q", m)
	}
	// A long Tamil text: under 4,000 letters, but each link would be far over 2,000 characters.
	if n := len([]rune(tamil)); n > 4000 || len(tamil) > 2000 {
		t.Fatalf("the Tamil text has %d letters", n)
	}
	if got[1].Chat != nil || got[1].Mail != "" {
		t.Errorf("a long Tamil text still offers WhatsApp %+v or Email %q", got[1].Chat, got[1].Mail)
	}
	// Chat links switched off: no WhatsApp link; Email still works, with no address when there is none.
	if got[2].Chat != nil || !strings.HasPrefix(got[2].Mail, "mailto:?subject=") {
		t.Errorf("with chat links off: %+v", got[2])
	}
	// No phone: the link asks which contact.
	if c := got[3].Chat; c == nil || !strings.HasPrefix(c.Web, "https://wa.me/?text=") || !strings.HasPrefix(c.App, "whatsapp://send?text=") {
		t.Errorf("the WhatsApp link with no phone = %+v", c)
	}
}
