// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	examplePlaybooks = "../../examples/playbooks"
	exampleMasters   = "../../examples/masters"
)

func TestLoadLibraryExamples(t *testing.T) {
	lib, err := LoadLibrary(examplePlaybooks, exampleMasters)
	if err != nil {
		t.Fatal(err)
	}
	if lib.Playbooks["job-evaluation"] == nil || lib.Masters["client"] == nil || lib.Masters["work_type"] == nil {
		t.Errorf("library = %+v, want job-evaluation, client and work_type", lib)
	}
}

func TestLoadLibraryFindsMistakes(t *testing.T) {
	tests := []struct {
		name, file, old, new, want string
	}{
		{"unknown master kind", "job-evaluation.yaml", "kind: work_type", "kind: worktype", `unknown master kind "worktype"`},
		{"prefill from missing master field", "job-evaluation.yaml", "standard_fee: standard_fee", "standard_fee: std_fee", `work_type has no field "std_fee"`},
		{"rule uses missing master field", "job-evaluation.yaml", "nature_of_work.is_new", "nature_of_work.is_fresh", `work_type has no field "is_fresh"`},
		{"prefill type mismatch", "work_type.yaml", "Estimated hours, type: number", "Estimated hours, type: text", "is text, but est_hours is number"},
		{"master field uses built-in name", "client.yaml", "key: phone,", "key: name,", "key and name are built in"},
		{"master field unknown type", "client.yaml", "Email, type: text", "Email, type: mail", `unknown type "mail"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pbDir, mDir := t.TempDir(), t.TempDir()
			copyExamples(t, examplePlaybooks, pbDir, tt.file, tt.old, tt.new)
			copyExamples(t, exampleMasters, mDir, tt.file, tt.old, tt.new)
			if _, err := LoadLibrary(pbDir, mDir); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("LoadLibrary() = %v\nwant a problem mentioning %q", err, tt.want)
			}
		})
	}
}

func TestLoadLibraryRejectsDuplicates(t *testing.T) {
	mDir := t.TempDir()
	copyExamples(t, exampleMasters, mDir, "", "", "")
	data, err := os.ReadFile(filepath.Join(exampleMasters, "client.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mDir, "client-copy.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLibrary(examplePlaybooks, mDir); err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Errorf("LoadLibrary() = %v, want a duplicate master kind error", err)
	}
}

func TestLoadLibraryMissingFolder(t *testing.T) {
	if _, err := LoadLibrary("no-such-folder", exampleMasters); err == nil {
		t.Error("LoadLibrary accepted a missing folder")
	}
}

// copyExamples copies the .yaml files from one folder to another. In the file named file, it
// replaces old with new, and stops the test if old is not found.
func copyExamples(t *testing.T, from, to, file, old, new string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(from, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(path) == file {
			changed := strings.Replace(string(data), old, new, 1)
			if changed == string(data) {
				t.Fatalf("test setup: %q not found in %s", old, file)
			}
			data = []byte(changed)
		}
		if err := os.WriteFile(filepath.Join(to, filepath.Base(path)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
