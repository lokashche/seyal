// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"encoding/json"
	"testing"

	"seyal/internal/workitem"
)

// People and roles events are skipped by name, whatever their payload: they never add a work item
// to the replay or stop it.
func TestPeopleEventsAreSkipped(t *testing.T) {
	w := expected{items: map[string]workitem.Item{}}
	for kind, payload := range map[string]string{
		"person.account_linked":        `{"how":"operator"}`,
		"person.logged_out_everywhere": `{"sessions":2}`,
		"person.odd":                   `["not an object"]`,
		"role.odd":                     `{"item":{"ID":"made-up"}}`,
	} {
		if err := w.apply(kind, "subject", "", json.RawMessage(payload)); err != nil {
			t.Errorf("%s %s: %v", kind, payload, err)
		}
	}
	if len(w.items) != 0 {
		t.Errorf("people events added items: %v", w.items)
	}
}

// A snapshot written before D70 has no hand-over keys; it reads as empty, which matches the empty
// columns of an old item.
func TestOldSnapshotHasNoHandOver(t *testing.T) {
	w := expected{items: map[string]workitem.Item{}}
	old := `{"change":{"assignee_id":"p-1"},"item":{"id":"i-1","number":"J-2026-0001","assignee_id":"p-1","version":1}}`
	if err := w.apply("item.assigned", "i-1", "", json.RawMessage(old)); err != nil {
		t.Fatal(err)
	}
	it := w.items["i-1"]
	if it.HeldSince != "" || it.HandoffFrom != "" || it.HandoffNote != "" {
		t.Errorf("old snapshot read as %+v, want no hand-over", it)
	}
	if reason := diffItem(it, workitem.Item{ID: "i-1", Number: "J-2026-0001", AssigneeID: "p-1", Version: 1}); reason != "" {
		t.Errorf("old snapshot against an old row: %s", reason)
	}
}
