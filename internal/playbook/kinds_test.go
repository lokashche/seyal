// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import "testing"

// TestWithDetails: missing details are added at the end, the list's own details stay, and the new
// definition reads back the same (labels with brackets, quotes and non-English text included).
func TestWithDetails(t *testing.T) {
	mine, err := ParseMasterKind([]byte("kind: work_type\nlabel: Work type\nfields:\n" +
		"  - { key: fee, label: \"Standard fee (\u20b9)\", type: money }\n" +
		"  - { key: city, label: \u0b8a\u0bb0\u0bcd, type: text }\n"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := ParseMasterKind([]byte("kind: work_type\nlabel: Work type\nfields:\n" +
		"  - { key: fee, label: Standard fee, type: money }\n" +
		"  - { key: fixed_price, label: \"Fixed price (\\\"same for all\\\")\", type: bool }\n" +
		"  - { key: level, label: Level, type: choice, options: [low, high] }\n"))
	if err != nil {
		t.Fatal(err)
	}
	missing := MissingDetails(mine, tmpl)
	if len(missing) != 2 || missing[0].Key != "fixed_price" || missing[1].Key != "level" {
		t.Fatalf("missing = %+v, want fixed_price and level", missing)
	}
	merged, err := mine.WithDetails(missing)
	if err != nil {
		t.Fatalf("merged definition doesn't read back: %v", err)
	}
	keys := ""
	for _, f := range merged.Fields {
		keys += f.Key + " "
	}
	if keys != "fee city fixed_price level " || merged.Fields[0].Label != "Standard fee (\u20b9)" ||
		merged.Fields[1].Label != "\u0b8a\u0bb0\u0bcd" || merged.Fields[2].Label != `Fixed price ("same for all")` ||
		len(merged.Fields[3].Options) != 2 {
		t.Errorf("merged = %+v", merged.Fields)
	}
	if len(MissingDetails(merged, tmpl)) != 0 {
		t.Error("still missing details after merging")
	}
}
