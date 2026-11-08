// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"testing"

	"seyal/internal/playbook"
)

// TestShareDigest: the fingerprint of the words Seyal offered matches the same words as a browser's
// text box gives them back (line ends as "\n"), and differs for any real change.
func TestShareDigest(t *testing.T) {
	offered := "Dear Acme Traders,\r\nplease send the papers.\rThanks"
	if ShareDigest(offered) != ShareDigest("Dear Acme Traders,\nplease send the papers.\nThanks") {
		t.Error("the same words with other line ends read as changed")
	}
	if ShareDigest(offered) == ShareDigest("Dear Acme Traders,\nplease send the papers.\nThanks!") {
		t.Error("changed words read as the same")
	}
	if len(ShareDigest("")) != 64 {
		t.Error("the fingerprint isn't SHA-256 in hex")
	}
}

// TestContactDetails: a phone becomes the digits a chat link wants, or nothing (the link then asks
// which contact); an email is used only when it is a plain address, so a value like "?bcc=" can't add
// hidden receivers to the Email button (D92).
func TestContactDetails(t *testing.T) {
	for phone, want := range map[string]string{
		"+91 00000 00000":         "910000000000",
		"+91-00000-00001":         "910000000001",
		"+44 (20) 0000.0000":      "442000000000",
		"(044) 0000 0000":         "", // no + and country code: it could open a stranger's chat
		"00000 00000":             "",
		"0000 000":                "", // 7 digits: too short
		"+91 0000 0000 0000 0000": "", // 18 digits: too long
		"00000 00000 ext 12":      "", // letters: not just a number
		"+91 00000 00000\n1":      "", // a line break
		"":                        "",
	} {
		if got := ChatDigits(phone); got != want {
			t.Errorf("ChatDigits(%q) = %q; want %q", phone, got, want)
		}
	}
	for email, want := range map[string]string{
		"accounts@sundaram.example":             "accounts@sundaram.example",
		" office@acme-traders.example ":         "office@acme-traders.example",
		"a@b.example?bcc=x@y.example":           "",
		"a@b.example&cc=x@y.example":            "",
		"a@b.example,x@y.example":               "",
		"a%40b@b.example":                       "",
		"Acme Traders <office@acme.example>":    "",
		"office@acme.example\nbcc: x@y.example": "",
		"no-at-sign.example":                    "",
		"office@localhost":                      "",
		"":                                      "",
	} {
		if got := PlainEmail(email); got != want {
			t.Errorf("PlainEmail(%q) = %q; want %q", email, got, want)
		}
	}
}

// TestContactOnlyOutside: a colleague message never gets a contact, even if it names one (the process
// check refuses that; this is the second guard). The session has no database: reaching for the list
// would fail the test.
func TestContactOnlyOutside(t *testing.T) {
	s := session{p: &playbook.Playbook{Fields: []playbook.Field{{Key: "party", Type: "master", Kind: "party"}}}}
	it := Item{Fields: map[string]any{"party": "acme-traders"}}
	phone, email, err := s.contact(playbook.ShareMessage{ID: "nudge", To: playbook.ShareColleague, Contact: "party"}, it)
	if phone != "" || email != "" || err != nil {
		t.Errorf("a colleague message's contact = %q, %q, %v; want none", phone, email, err)
	}
}
