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
