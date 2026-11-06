// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import "testing"

// TestConfirmWord: only "yes" or "no" (any case, spaces around, one full stop) answer a suggestion.
// Words that other yes/no questions still take ("ok", "y", "yeah", "1", "0") don't (D74).
func TestConfirmWord(t *testing.T) {
	for _, tt := range []struct {
		text    string
		yes, ok bool
	}{
		{"yes", true, true},
		{"Yes", true, true},
		{"YES", true, true},
		{"  yes  ", true, true},
		{"yes.", true, true},
		{" Yes. ", true, true},
		{"yes .", true, true},
		{"no", false, true},
		{"No.", false, true},
		{" NO ", false, true},
		// Not a clear yes or no to a suggestion.
		{"ok", false, false},
		{"okay", false, false},
		{"y", false, false},
		{"n", false, false},
		{"yeah", false, false},
		{"yep", false, false},
		{"nope", false, false},
		{"true", false, false},
		{"1", false, false},
		{"0", false, false},
		{"yes!", false, false},
		{"yes..", false, false},
		{"yes please", false, false},
		{"no, wait", false, false},
		{"", false, false},
		{".", false, false},
	} {
		yes, ok := confirmWord(tt.text)
		if yes != tt.yes || ok != tt.ok {
			t.Errorf("confirmWord(%q) = %v, %v; want %v, %v", tt.text, yes, ok, tt.yes, tt.ok)
		}
	}
	// Other yes/no questions keep their old words.
	for _, text := range []string{"ok", "y", "yeah", "1"} {
		if yes, err := parseYesNo(text); err != nil || !yes {
			t.Errorf("parseYesNo(%q) = %v, %v; want yes", text, yes, err)
		}
	}
	for _, text := range []string{"n", "nope", "0"} {
		if yes, err := parseYesNo(text); err != nil || yes {
			t.Errorf("parseYesNo(%q) = %v, %v; want no", text, yes, err)
		}
	}
}
