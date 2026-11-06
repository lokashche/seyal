// SPDX-License-Identifier: AGPL-3.0-or-later

package hours

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]Hours{
		"09:00-19:00":    {9 * 60, 19 * 60},
		" 08:30 - 24:00": {8*60 + 30, 24 * 60},
		"00:00-24:00":    {0, 24 * 60},
	} {
		h, err := Parse(in)
		if err != nil || h != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, h, err, want)
		}
	}
	for _, bad := range []string{"", "9-7", "09:00", "19:00-09:00", "09:00-09:00", "24:00-24:00", "25:00-26:00", "09:60-19:00", "9:00-19:00"} {
		if h, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = %v, want a problem", bad, h)
		}
	}
	if h, _ := Parse("08:30-24:00"); h.String() != "08:30-24:00" || h.FromText() != "08:30" || h.UntilText() != "24:00" {
		t.Errorf("written back as %s (%s, %s)", h, h.FromText(), h.UntilText())
	}
	if _, err := Parse(Default); err != nil {
		t.Errorf("the default %q: %v", Default, err)
	}
}
