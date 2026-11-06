// SPDX-License-Identifier: AGPL-3.0-or-later

package setup

import (
	"fmt"
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
	if f.Business.Currency != "INR" || len(f.People) != 11 || len(f.Master["work_type"]) != 4 ||
		!slices.Equal(f.Business.DaysOff, []string{"sunday"}) {
		t.Errorf("demo file = %+v, %d people", f.Business, len(f.People))
	}
	days, problems := f.Business.days()
	if !slices.Equal(days, []int16{0}) || problems != nil {
		t.Errorf("demo days off = %v, %v; want [0] (Sunday)", days, problems)
	}
}

// TestWorkingDaysInTheFile: days off and holidays are read in any case and stored sorted (D75,
// decision 1).
func TestWorkingDaysInTheFile(t *testing.T) {
	b := Business{DaysOff: []string{"Saturday", " sunday"}, Holidays: []string{"2026-12-25", "2026-11-09"}}
	if days, problems := b.days(); !slices.Equal(days, []int16{0, 6}) || problems != nil {
		t.Errorf("days = %v, %v; want [0 6]", days, problems)
	}
	if dates, problems := b.holidays(); !slices.Equal(dates, []string{"2026-11-09", "2026-12-25"}) || problems != nil {
		t.Errorf("holidays = %v, %v; want them sorted", dates, problems)
	}
	if days, _ := (Business{}).days(); days == nil {
		t.Error("no days off must be an empty list, not nil (Postgres would read nil as NULL)")
	}
	many := Business{}
	for i := range MaxHolidays + 1 {
		many.Holidays = append(many.Holidays, fmt.Sprintf("2027-%02d-%02d", 1+i/28, 1+i%28))
	}
	if _, problems := many.holidays(); len(problems) != 1 || !strings.Contains(problems[0], "at most 100 dates") {
		t.Errorf("101 holidays: %v; want them refused", problems)
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
		{"no processes line", "processes: [job-evaluation, other-work, monthly-return]\n", "", "list the processes"},
		{"unknown process", "processes: [job-evaluation, other-work, monthly-return]", "processes: [job-eval]", `process "job-eval": there is no such template`},
		{"unknown list", "processes: [job-evaluation, other-work, monthly-return]", "processes: [job-evaluation]\nlists: [members]", `list "members": there is no such template`},
		{"own file missing", "processes: [job-evaluation, other-work, monthly-return]", "processes: [mine.yaml]", "process mine.yaml"},
		{"unknown field", "standard_fee: 12000", "std_fee: 12000", `no field "std_fee"`},
		{"wrong type", "standard_fee: 5000", "standard_fee: five thousand", "standard_fee must be"},
		{"typo in a key", "fiscal_year_start_month: 4", "fiscal_year_start_month: 4\n  colour: blue", "colour"},
		{"bad day name", "days_off: [sunday]", "days_off: [sundy]", `days_off: "sundy" is not a day of the week`},
		{"a day twice", "days_off: [sunday]", "days_off: [sunday, Sunday]", "days_off: sunday is listed twice"},
		{"all days off", "days_off: [sunday]",
			"days_off: [sunday, monday, tuesday, wednesday, thursday, friday, saturday]", "at least one day a week must be a working day"},
		{"bad holiday", "days_off: [sunday]", "days_off: [sunday]\n  holidays: [2026-02-30]", `holidays: "2026-02-30" is not a date`},
		{"a holiday twice", "days_off: [sunday]", "days_off: [sunday]\n  holidays: [2026-11-09, 2026-11-09]", "holidays: 2026-11-09 is listed twice"},
		{"bad chase line", "days_off: [sunday]", "days_off: [sunday]\n  chase: stop", `chase "stop": write "chase: paused"`},
		{"bad phone-note switch", "notifications: on", "notifications: yes please", `notifications "yes please": write "notifications: on"`},
		{"phone-note hours the wrong way round", `notify_hours: "09:00-19:00"`, `notify_hours: "19:00-09:00"`, "the start must be before the end"},
		{"phone-note hours not times", `notify_hours: "09:00-19:00"`, `notify_hours: "9-7"`, "write it as 09:00-19:00"},
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
