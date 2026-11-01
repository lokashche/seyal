// SPDX-License-Identifier: AGPL-3.0-or-later

package setup

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"seyal/internal/playbook"
)

const demoPath = "../../examples/seed/demo.yaml"

func library(t *testing.T) *playbook.Library {
	t.Helper()
	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	return lib
}

func TestDemoFileIsValid(t *testing.T) {
	f, err := Load(demoPath, library(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Business.Currency != "INR" || len(f.People) != 6 || len(f.Master["work_type"]) != 4 {
		t.Errorf("demo file = %+v", f.Business)
	}
}

func TestCheckFindsMistakes(t *testing.T) {
	demo, err := os.ReadFile(demoPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, old, new, want string }{
		{"bad slug", "slug: demo", "slug: Demo Firm", "business slug"},
		{"bad currency", "currency: INR", "currency: rupees", "currency"},
		{"unknown time zone", "timezone: Asia/Kolkata", "timezone: Mars/Base", "time zone"},
		{"bad fiscal month", "fiscal_year_start_month: 4", "fiscal_year_start_month: 13", "fiscal_year_start_month"},
		{"role not listed", "roles: [ops]", "roles: [operations]", `role "operations" is not listed`},
		{"duplicate email", "priya@kaveri-demo.example", "arjun@kaveri-demo.example", "needs its own email"},
		{"unknown person", `maker: "@karthik"`, `maker: "@kavya"`, "unknown person @kavya"},
		{"person not as @key", `checker: "@fatima"`, `checker: Fatima`, "must be written as @key"},
		{"unknown master kind", "  work_type:\n", "  worktype:\n", `list "worktype" is used but not defined`},
		{"no processes line", "processes: [job-evaluation]\n", "", "list the processes"},
		{"unknown process", "processes: [job-evaluation]", "processes: [job-eval]", `process "job-eval": there is no such template`},
		{"unknown list", "processes: [job-evaluation]", "processes: [job-evaluation]\nlists: [members]", `list "members": there is no such template`},
		{"own file missing", "processes: [job-evaluation]", "processes: [mine.yaml]", "process mine.yaml"},
		{"unknown field", "standard_fee: 12000", "std_fee: 12000", `no field "std_fee"`},
		{"wrong type", "standard_fee: 5000", "standard_fee: five thousand", "standard_fee must be"},
		{"typo in a key", "fiscal_year_start_month: 4", "fiscal_year_start_month: 4\n  colour: blue", "colour"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(string(demo), tt.old, tt.new, 1)
			if changed == string(demo) {
				t.Fatalf("test setup: %q not found", tt.old)
			}
			path := filepath.Join(t.TempDir(), "business.yaml")
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, library(t)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load = %v, want a problem mentioning %q", err, tt.want)
			}
		})
	}
}

// TestOwnFilesAndChoices: a business takes only the processes and lists it names, and its own files
// can sit next to its set-up file instead of in the server's template folders.
func TestOwnFilesAndChoices(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("court.yaml", "kind: court\nlabel: Court\nfields:\n  - { key: surface, label: Surface, type: text }\n")
	write("booking.yaml", `id: slot-booking
version: "1"
description: Book a court.
numbering: "B-{seq:4}"
fields:
  - { key: court, label: Court, type: master, kind: court, required: true, ask: "Which court?" }
states: [requested, confirmed]
initial: requested
transitions:
  - { action: confirm, from: [requested], to: confirmed, when: required_filled }
`)
	write("business.yaml", `business: { slug: turf-demo, name: Green Turf (demo), currency: INR, timezone: Asia/Kolkata, fiscal_year_start_month: 4 }
processes: [booking.yaml]
lists: [court.yaml]
master:
  court:
    - { key: c1, name: Court 1, fields: { surface: grass } }
`)
	f, err := Load(filepath.Join(dir, "business.yaml"), library(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(f.chosen.Playbooks)); !slices.Equal(got, []string{"slot-booking"}) {
		t.Errorf("processes = %v, want only slot-booking", got)
	}
	if got := slices.Sorted(maps.Keys(f.chosen.Masters)); !slices.Equal(got, []string{"court"}) {
		t.Errorf("lists = %v, want only court (nothing from the samples)", got)
	}
}

// TestStartedProcessesComeAlong: choosing a process that starts another as linked work brings that
// one along from the templates.
func TestStartedProcessesComeAlong(t *testing.T) {
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Load(demoPath, lib)
	if err != nil {
		t.Fatal(err)
	}
	if f.chosen.Playbooks["requirement-study"] == nil {
		t.Errorf("chosen = %v, want requirement-study brought along by job-evaluation", slices.Sorted(maps.Keys(f.chosen.Playbooks)))
	}
}
