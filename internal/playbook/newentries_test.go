// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

// TestNewEntries: a list may preset values for entries added from the chat; the presets are checked
// against the list's details when the definition is loaded.
func TestNewEntries(t *testing.T) {
	base := "kind: member\nlabel: Member\nfields:\n" +
		"  - { key: is_new, label: New, type: bool }\n" +
		"  - { key: level, label: Level, type: choice, options: [low, high] }\n" +
		"  - { key: visits, label: Visits, type: number }\n" +
		"  - { key: joined_on, label: Joined on, type: date }\n"
	m, err := ParseMasterKind([]byte(base + "new_entries: { is_new: true, level: low, visits: 0 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.NewEntryFields(); got["is_new"] != true || got["level"] != "low" || got["visits"] != 0.0 {
		t.Errorf("NewEntryFields = %v", got)
	}
	for presets, want := range map[string]string{
		"{ is_newer: true }":        `unknown field "is_newer"`,
		"{ is_new: yes please }":    "give a bool value",
		"{ level: medium }":         "give a choice value",
		"{ joined_on: 2026-01-01 }": "can only preset",
		"{ visits: lots }":          "give a number value",
	} {
		if _, err := ParseMasterKind([]byte(base + "new_entries: " + presets + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("new_entries %s = %v, want %q", presets, err, want)
		}
	}
}
