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
