// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"strings"
	"testing"

	"seyal/internal/playbook"
)

// TestCheckProcess: a process may send only to an other app the business lists, only fields on that
// app's own list, and the item's link only where the app allows it (D96). The problems name the app and
// the field, never an address.
func TestCheckProcess(t *testing.T) {
	p, err := playbook.LoadFile("../../examples/seed/job-evaluation-send.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sheet := Target{Name: "jobs-sheet", Label: "Sheet one", URL: "https://hook.example/secret-path-0000",
		Fields: []string{"client", "nature_of_work", "fee_agreed", "advance", "timeline_days"}}
	if problems := CheckProcess([]Target{sheet}, p); len(problems) != 0 {
		t.Errorf("the demo sample against the demo's app: %v", problems)
	}
	none := CheckProcess(nil, p)
	if len(none) != 1 || !strings.Contains(none[0], "lists no other app called jobs-sheet") {
		t.Errorf("no app: %v", none)
	}
	short := sheet
	short.Fields = []string{"client"}
	few := CheckProcess([]Target{short}, p)
	if len(few) != 4 || !strings.Contains(few[0], "jobs-sheet may not receive nature_of_work") {
		t.Errorf("an app that may receive only client: %v", few)
	}
	for _, pr := range append(none, few...) {
		if strings.Contains(pr, "secret-path") || strings.Contains(pr, "hook.example") {
			t.Errorf("a problem shows the address: %q", pr)
		}
	}
	if s := Sends(p); len(s) != 1 || s[0].State != "approved" {
		t.Errorf("Sends = %+v", s)
	}
	for i, a := range p.On["approved"] {
		if a.Do == "send" {
			p.On["approved"][i].Link = true
		}
	}
	if got := CheckProcess([]Target{sheet}, p); len(got) != 1 || !strings.Contains(got[0], "may not receive the item's link") {
		t.Errorf("a link the app doesn't allow: %v", got)
	}
}
