// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"seyal/internal/playbook"
)

// runCheck loads and checks every playbook and master kind, and prints a summary.
func runCheck(out io.Writer) error {
	playbooksDir, mastersDir := libraryDirs()
	lib, err := playbook.LoadLibrary(playbooksDir, mastersDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "playbooks in %s\n", playbooksDir)
	for _, id := range slices.Sorted(maps.Keys(lib.Playbooks)) {
		p := lib.Playbooks[id]
		fmt.Fprintf(out, "  ok  %s, version %s (%d fields, %d states)\n", p.ID, p.Version, len(p.Fields), len(p.States))
	}
	fmt.Fprintf(out, "master data kinds in %s\n", mastersDir)
	for _, kind := range slices.Sorted(maps.Keys(lib.Masters)) {
		fmt.Fprintf(out, "  ok  %s (%d fields)\n", kind, len(lib.Masters[kind].Fields))
	}
	return nil
}

// libraryDirs returns the playbook and master-data folders from the settings, or the samples.
func libraryDirs() (playbooks, masters string) {
	playbooks, masters = os.Getenv("SEYAL_PLAYBOOKS_DIR"), os.Getenv("SEYAL_MASTERS_DIR")
	if playbooks == "" {
		playbooks = filepath.Join("examples", "playbooks")
	}
	if masters == "" {
		masters = filepath.Join("examples", "masters")
	}
	return playbooks, masters
}
