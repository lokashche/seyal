// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"testing"

	"seyal/internal/playbook"
)

// TestReachable: the "will send" line shows on every step that can still lead to the sending state
// (D96): from the draft and from approvals, the approved step is ahead; once approved or cancelled, it
// isn't any more.
func TestReachable(t *testing.T) {
	p, err := playbook.LoadFile("../../examples/seed/job-evaluation-send.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for from, want := range map[string]bool{"draft": true, "studying": true, "with_ops": true, "pending_approval": true,
		"approved": false, "cancelled": false} {
		if got := Reachable(p, from)["approved"]; got != want {
			t.Errorf("approved reachable from %s = %v, want %v", from, got, want)
		}
	}
}
