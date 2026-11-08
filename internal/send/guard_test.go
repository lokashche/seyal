// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGuard(t *testing.T) {
	for in, want := range map[string]string{
		`=HYPERLINK("https://evil.example","Acme")`: `'=HYPERLINK("https://evil.example","Acme")`,
		"+91 00000 00000": "'+91 00000 00000",
		"-5":              "'-5",
		"- a list line":   "'- a list line",
		"@SUM(A1)":        "'@SUM(A1)",
		"\t=1+1":          "'\t=1+1",
		"\r=1+1":          "'\r=1+1",
		"Acme Traders":    "Acme Traders",
		"J-2026-0007":     "J-2026-0007", // a dash inside is fine
		"'already":        "'already",
		"":                "",
		" =1":             " =1", // a sheet doesn't read it as a formula
	} {
		if got := Guard(in); got != want {
			t.Errorf("Guard(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuardAllLeavesNumbers(t *testing.T) {
	in := map[string]any{
		"name":  "=cmd|' /C calc'!A0",
		"minor": int64(-1500000),
		"days":  -21,
		"rate":  -0.5,
		"json":  json.Number("-7"),
		"ok":    true,
		"none":  nil,
		"nested": map[string]any{
			"key":  "@acme",
			"list": []any{"-x", 3, "plain"},
		},
		"row":  []any{"J-2026-0007", "+Acme", -15000, "INR"},
		"tags": []string{"\tx", "y"},
	}
	want := map[string]any{
		"name":  "'=cmd|' /C calc'!A0",
		"minor": int64(-1500000),
		"days":  -21,
		"rate":  -0.5,
		"json":  json.Number("-7"),
		"ok":    true,
		"none":  nil,
		"nested": map[string]any{
			"key":  "'@acme",
			"list": []any{"'-x", 3, "plain"},
		},
		"row":  []any{"J-2026-0007", "'+Acme", -15000, "INR"},
		"tags": []string{"'\tx", "y"},
	}
	got, err := GuardAll(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GuardAll =\n%#v\nwant\n%#v", got, want)
	}
	if in["name"] != "=cmd|' /C calc'!A0" {
		t.Error("GuardAll changed its input")
	}
	// After JSON, a negative number is still a number, not text.
	b, _ := json.Marshal(got)
	var back map[string]any
	json.Unmarshal(b, &back)
	if _, ok := back["minor"].(float64); !ok {
		t.Errorf("minor went out as %T", back["minor"])
	}
}

// labelled is a number type that writes itself as text (a label), like a state or choice name.
type labelled int

func (labelled) MarshalText() ([]byte, error) {
	return []byte(`=HYPERLINK("https://evil.example","x")`), nil
}

// jsonLabelled is the same with its own MarshalJSON.
type jsonLabelled float64

func (jsonLabelled) MarshalJSON() ([]byte, error) {
	return []byte(`"=HYPERLINK(\"https://evil.example\",\"x\")"`), nil
}

// Typed maps and lists (and named string types) must be guarded too, never passed through as they
// are: a sheet would read the formula in them as a live link.
func TestGuardAllTypedContainers(t *testing.T) {
	type name string
	const evil = `=HYPERLINK("https://evil.example","x")`
	for label, in := range map[string]any{
		"map[string]string":         map[string]string{"n": evil},
		"[]map[string]any":          []map[string]any{{"n": evil, "minor": -1500}},
		"[][]string":                [][]string{{"J-2026-0007", evil}},
		"map[string][]string":       map[string][]string{"n": {evil}},
		"a named string":            name(evil),
		"a struct":                  struct{ N string }{evil},
		"typed inside any":          map[string]any{"row": []map[string]string{{"n": evil}}},
		"a pointer to a string":     func() *string { s := evil; return &s }(),
		"a number with MarshalText": labelled(1),
		"a number with MarshalJSON": jsonLabelled(1),
		"MarshalText inside any":    map[string]any{"s": []any{labelled(2)}},
	} {
		got, err := GuardAll(in)
		if err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		b, _ := json.Marshal(got)
		if strings.Contains(string(b), `"=HYPERLINK`) || !strings.Contains(string(b), `"'=HYPERLINK`) {
			t.Errorf("%s: not guarded: %s", label, b)
		}
		if strings.Contains(string(b), `"-1500"`) {
			t.Errorf("%s: a number became text: %s", label, b)
		}
	}
	// A value that can't be JSON is an error, not a quiet pass.
	if _, err := GuardAll(map[string]any{"f": func() {}}); err == nil {
		t.Error("a func went through with no error")
	}
}
