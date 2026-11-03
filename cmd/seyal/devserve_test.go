// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChanged(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	before := map[string]time.Time{"web/static/app.css": t0, "internal/server/app.go": t0}
	for _, tt := range []struct {
		name  string
		after map[string]time.Time
		want  string
	}{
		{"nothing", map[string]time.Time{"web/static/app.css": t0, "internal/server/app.go": t0}, ""},
		{"saved", map[string]time.Time{"web/static/app.css": t0.Add(time.Second), "internal/server/app.go": t0}, "web/static/app.css"},
		{"new file", map[string]time.Time{"web/static/app.css": t0, "internal/server/app.go": t0, "web/templates/me.html": t0}, "web/templates/me.html"},
		{"deleted", map[string]time.Time{"web/static/app.css": t0}, "internal/server/app.go"},
	} {
		if got := changed(before, tt.after); got != tt.want {
			t.Errorf("%s: changed = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// The watcher must see the server's own files and not the tests, which don't change the server.
func TestSnapshotWatchesTheServer(t *testing.T) {
	t.Chdir("../..") // the repository root, where dev serve runs
	files := snapshot()
	for _, want := range []string{"cmd/seyal/main.go", "web/templates/layout.html", "web/static/app.css", "migrations/00014_login_links.sql"} {
		if _, ok := files[filepath.FromSlash(want)]; !ok {
			t.Errorf("not watched: %s", want)
		}
	}
	for path := range files {
		if strings.HasSuffix(path, "_test.go") {
			t.Errorf("a test file is watched: %s", path)
		}
	}
}
