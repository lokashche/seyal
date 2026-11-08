// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// The formula guard. A spreadsheet reads text that starts with =, +, -, @, a tab or a carriage
// return as a formula (or the start of one), so a name like =HYPERLINK(...) sent to a sheet would
// become a working link. Every text value that leaves Seyal for another app gets a ' in front when it
// starts with one of these. Numbers are never turned into text, so a negative amount stays a
// number. (OWASP "CSV injection".)

// formulaStarts are the first characters a spreadsheet may read as a formula.
const formulaStarts = "=+-@\t\r"

// Guard returns s with a ' in front when it starts like a formula, and s itself otherwise.
func Guard(s string) string {
	if s != "" && strings.IndexByte(formulaStarts, s[0]) >= 0 {
		return "'" + s
	}
	return s
}

// GuardAll returns v with Guard applied to every text inside it: a string, and the strings in maps
// (their values, not their keys) and lists, at any depth. Numbers, true and false, and nil come back
// unchanged. v is not changed; maps and lists are copied.
//
// It never lets text through unguarded. Any value that is not a plain string, number, bool, nil,
// map[string]any, []any or []string (a map[string]string, a []map[string]any, a named string type,
// a struct, a number type with its own MarshalJSON or MarshalText ...) is first turned into JSON and read back as those plain kinds, then guarded. So the
// result may have a different Go type, but the same JSON. A value that can't be turned into JSON is
// an error, never a quiet pass.
func GuardAll(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, json.Number:
		return v, nil
	case string:
		return Guard(x), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			g, err := GuardAll(val)
			if err != nil {
				return nil, err
			}
			out[k] = g
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			g, err := GuardAll(val)
			if err != nil {
				return nil, err
			}
			out[i] = g
		}
		return out, nil
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = Guard(s)
		}
		return out, nil
	}
	_, marshalsJSON := v.(json.Marshaler)
	_, marshalsText := v.(encoding.TextMarshaler)
	switch k := reflect.ValueOf(v).Kind(); {
	case marshalsJSON || marshalsText:
		// A type with its own JSON or text form may write text (a label), even when it is a
		// number underneath, so it goes through the JSON round trip below and gets guarded.
	case k >= reflect.Int && k <= reflect.Uint64, k == reflect.Float32, k == reflect.Float64, k == reflect.Bool:
		return v, nil // a number (or a named number type): never text
	}
	// Anything else: through JSON into plain kinds (numbers kept as json.Number, so they stay numbers).
	b, err := json.Marshal(v)
	if err != nil {
		return nil, errors.New("send: a value to send can't be turned into JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var plain any
	if err := dec.Decode(&plain); err != nil {
		return nil, errors.New("send: a value to send can't be read back from JSON")
	}
	return GuardAll(plain)
}
