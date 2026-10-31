// SPDX-License-Identifier: AGPL-3.0-or-later

package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seyal/internal/playbook"
)

const demoPath = "../../examples/seed/demo.yaml"

func library(t *testing.T) *playbook.Library {
	t.Helper()
	lib, err := playbook.LoadLibrary("../../examples/playbooks", "../../examples/masters")
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
		{"unknown master kind", "  work_type:\n", "  worktype:\n", `master kind "worktype"`},
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
