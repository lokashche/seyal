// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestResetSafety: a reset only reaches a database on this computer, only clears a folder inside
// this one, and does nothing unless the person types exactly "reset".
func TestResetSafety(t *testing.T) {
	local := "postgres://seyal_owner:x@localhost:5432/seyal?sslmode=disable"
	for _, tt := range []struct {
		urls []string
		ok   bool
	}{
		{[]string{local, "postgres://seyal_app:x@127.0.0.1:5432/seyal"}, true},
		{[]string{local, "postgres://seyal_app:x@db.example.com:5432/seyal"}, false},
		{[]string{local, ""}, false},
	} {
		if err := localOnly(tt.urls...); (err == nil) != tt.ok {
			t.Errorf("localOnly(%q) = %v, want ok %v", tt.urls, err, tt.ok)
		}
	}

	for answer, want := range map[string]bool{"reset\n": true, " reset \r\n": true, "yes\n": false, "Reset\n": false, "": false} {
		if got := confirmed(strings.NewReader(answer), "reset"); got != want {
			t.Errorf("confirmed(%q) = %v, want %v", answer, got, want)
		}
	}

	for _, dir := range []string{"../elsewhere", "/var/seyal/media", `C:\seyal\media`} {
		if err := clearMedia(dir); err == nil {
			t.Errorf("clearMedia(%q) should refuse", dir)
		}
	}

	t.Setenv("SEYAL_OWNER_DATABASE_URL", local)
	t.Setenv("SEYAL_DATABASE_URL", local)
	var out bytes.Buffer
	if err := devReset(strings.NewReader("no\n"), &out); err != nil || !strings.Contains(out.String(), "Nothing was changed.") {
		t.Errorf("answering no = %v, %q", err, out.String())
	}
}
