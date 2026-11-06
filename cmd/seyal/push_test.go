// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"

	"seyal/internal/push"
)

func TestPushKeys(t *testing.T) {
	keys := func() (string, string) {
		t.Helper()
		var out bytes.Buffer
		if err := run([]string{"push", "keys"}, &out); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("push keys printed %d lines, want 2:\n%s", len(lines), out.String())
		}
		pub, ok1 := strings.CutPrefix(lines[0], "SEYAL_PUSH_PUBLIC_KEY=")
		priv, ok2 := strings.CutPrefix(lines[1], "SEYAL_PUSH_PRIVATE_KEY=")
		if !ok1 || !ok2 {
			t.Fatalf("push keys printed:\n%s", out.String())
		}
		return pub, priv
	}
	pub, priv := keys()
	if _, err := push.ParseKeys(pub, priv); err != nil {
		t.Errorf("the printed keys don't read back: %v", err)
	}
	if pub2, priv2 := keys(); pub2 == pub || priv2 == priv {
		t.Error("two runs printed the same keys")
	}

	for _, args := range [][]string{{"push"}, {"push", "nope"}, {"push", "keys", "extra"}} {
		var out bytes.Buffer
		if err := run(args, &out); err == nil {
			t.Errorf("run(%q) gave no error", args)
		}
	}
}

// TestPushSwitch: SEYAL_PUSH is off unless it says "on", and "on" needs right keys and a contact.
// Errors never show a key.
func TestPushSwitch(t *testing.T) {
	keys, err := push.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	other, err := push.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	good := []string{"SEYAL_PUSH_PUBLIC_KEY", keys.Public(), "SEYAL_PUSH_PRIVATE_KEY", keys.Private(),
		"SEYAL_PUSH_CONTACT", "mailto:office@kaveri-demo.example"}
	for _, off := range []string{"", "off"} {
		if key, err := pushKey(env(append([]string{"SEYAL_PUSH", off}, good...)...)); key != "" || err != nil {
			t.Errorf("SEYAL_PUSH=%q = %q, %v; want off", off, key, err)
		}
	}
	if key, err := pushKey(env(append([]string{"SEYAL_PUSH", "on"}, good...)...)); key != keys.Public() || err != nil {
		t.Errorf("SEYAL_PUSH=on = %q, %v; want the public key", key, err)
	}
	for name, kv := range map[string][]string{
		"a typo":  append([]string{"SEYAL_PUSH", "yes"}, good...),
		"no keys": {"SEYAL_PUSH", "on", "SEYAL_PUSH_CONTACT", "mailto:office@kaveri-demo.example"},
		"keys of two pairs": {"SEYAL_PUSH", "on", "SEYAL_PUSH_PUBLIC_KEY", other.Public(), "SEYAL_PUSH_PRIVATE_KEY", keys.Private(),
			"SEYAL_PUSH_CONTACT", "mailto:office@kaveri-demo.example"},
		"no contact": {"SEYAL_PUSH", "on", "SEYAL_PUSH_PUBLIC_KEY", keys.Public(), "SEYAL_PUSH_PRIVATE_KEY", keys.Private()},
	} {
		_, err := pushKey(env(kv...))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), keys.Private()) || strings.Contains(err.Error(), keys.Public()) {
			t.Errorf("%s: the error shows a key: %v", name, err)
		}
	}
}
