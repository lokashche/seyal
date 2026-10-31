// SPDX-License-Identifier: AGPL-3.0-or-later

// Package core_test checks rules that apply to all of the engine in internal/.
package core_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// businessWords must never appear in the engine's code, not even in comments. They belong in
// playbooks and master data. Tests may use them, because tests load the sample playbooks.
var businessWords = regexp.MustCompile(
	`(?i)\b(jobs?|clients?|customers?|fees?|gym|turf|invoices?|gst|whatsapp|telegram)\b`)

func TestCoreHasNoBusinessWords(t *testing.T) {
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if w := businessWords.FindString(line); w != "" {
				t.Errorf("%s:%d: business word %q in the engine; it belongs in a playbook or master data", path, i+1, w)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files checked; is the test in the right folder?")
	}
}
