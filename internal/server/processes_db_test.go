//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
)

func TestSettingsProcesses(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	proc := base + "/settings/processes/job-evaluation"
	sample, err := os.ReadFile("../testdata/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	save := func(source string) (int, string) {
		code, body, _ := s.post(l, proc, url.Values{"action": {"save"}, "source": {source}}, nil)
		return code, body
	}
	// startJob starts a job as Arjun and returns the version of the process it runs on.
	startJob := func() string {
		a := s.logIn(arjun)
		_, home, _ := s.get(a, base+"/")
		m := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)
		if m == nil {
			t.Fatal("Tell Seyal offers no process")
		}
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"New job"}}, nil)
		id := h.Get("Location")[strings.LastIndex(h.Get("Location"), "/")+1:]
		var version string
		err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT pv.version FROM work_items w
				JOIN playbook_versions pv ON pv.tenant_id = w.tenant_id AND pv.id = w.playbook_version_id
				WHERE w.id::text = $1`, id).Scan(&version)
		})
		if err != nil {
			t.Fatal(err)
		}
		return version
	}

	t.Run("listed and described in plain words", func(t *testing.T) {
		_, list, _ := s.get(l, base+"/settings/processes")
		if !strings.Contains(list, "Job evaluation") || !strings.Contains(list, "version 0.1") || !strings.Contains(list, "in use") {
			t.Fatal("process list doesn't show Job evaluation 0.1 in use")
		}
		_, page, _ := s.get(l, proc)
		for _, want := range []string{
			"Owner (role owner): when Fee agreed is less than Standard fee, or Nature of work → New work type (needs owner approval) is yes, or Integrity check is fail",
			"Submit: from draft to pending approval, once every required field is filled",
			"Seyal asks: “What fee did we agree?”",
			"When approved:",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("process page is missing %q", want)
			}
		}
	})

	before := startJob()

	t.Run("same version with different content is refused", func(t *testing.T) {
		code, body := save(strings.Replace(string(sample), "Evaluate a new piece", "Check a new piece", 1))
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "Change the version line") {
			t.Errorf("changed content, same version = %d: %s", code, problem(body))
		}
		if !strings.Contains(body, "Check a new piece") {
			t.Error("the edited text was lost after the refusal")
		}
	})

	t.Run("mistakes are explained before anything goes live", func(t *testing.T) {
		broken := strings.NewReplacer(`version: "0.1"`, `version: "0.9"`, "to: pending_approval", "to: pending_aproval").Replace(string(sample))
		if code, body := save(broken); code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "pending_aproval") {
			t.Errorf("broken definition = %d: %s", code, problem(body))
		}
		renamed := strings.Replace(string(sample), "id: job-evaluation", "id: something-else", 1)
		if code, body := save(renamed); code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "must stay") {
			t.Errorf("renamed id = %d: %s", code, problem(body))
		}
	})

	t.Run("a new version is live at once; started work keeps its version", func(t *testing.T) {
		v2 := strings.NewReplacer(`version: "0.1"`, `version: "0.2"`, "    - { slot: ops, role: ops }\n", "").Replace(string(sample))
		code, body := save(v2)
		if code != http.StatusOK || !strings.Contains(body, "Version 0.2 saved and in use") {
			t.Fatalf("save 0.2 = %d: %s", code, problem(body))
		}
		if after := startJob(); after != "0.2" {
			t.Errorf("new job runs on %s, want 0.2", after)
		}
		if before != "0.1" {
			t.Errorf("the earlier job runs on %s, want 0.1", before)
		}
	})

	t.Run("go back to an older version", func(t *testing.T) {
		_, page, _ := s.get(l, proc)
		m := regexp.MustCompile(`name="version" value="([^"]+)"`).FindStringSubmatch(page)
		if m == nil {
			t.Fatal("no older version to choose")
		}
		s.post(l, proc, url.Values{"action": {"use"}, "version": {m[1]}}, nil)
		if v := startJob(); v != "0.1" {
			t.Errorf("after choosing 0.1, new job runs on %s", v)
		}
	})

	t.Run("switched off: not offered for new work", func(t *testing.T) {
		a := s.logIn(arjun)
		_, home, _ := s.get(a, base+"/")
		pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
		s.post(l, proc, url.Values{"action": {"disable"}}, nil)
		_, home, _ = s.get(a, base+"/")
		if strings.Contains(home, `name="playbook"`) {
			t.Error("a switched-off process is still offered")
		}
		code, body, _ := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"Sneaky job"}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "isn't in use") {
			t.Errorf("starting work on a switched-off process = %d: %s", code, problem(body))
		}
		s.post(l, proc, url.Values{"action": {"enable"}}, nil)
	})

	s.tablesMatchLedger(t)
}

// TestTemplateNeedsListDetails: when a newer template version needs a detail the business's list
// lacks, the process page says so and one button adds the detail (keeping the list's own changes)
// and switches to that version.
func TestTemplateNeedsListDetails(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)
	proc := base + "/settings/processes/job-evaluation"
	b, err := os.ReadFile("../testdata/playbooks/job-evaluation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The business runs its own older version that doesn't use "is new", and its work type list has
	// lost that detail but gained one of its own.
	older := strings.Replace(string(b), `version: "0.1"`, `version: "0.0"`, 1)
	older = strings.Replace(older, "          - { field: nature_of_work.is_new, eq: true }\n", "", 1)
	if code, body, _ := s.post(l, proc, url.Values{"action": {"save"}, "source": {older}}, nil); code != http.StatusOK {
		t.Fatalf("save the older version = %d: %s", code, problem(body))
	}
	w, err := os.ReadFile("../../examples/masters/work_type.yaml")
	if err != nil {
		t.Fatal(err)
	}
	list := strings.Replace(string(w), "  - { key: is_new,", "  - { key: region, label: Region, type: text }\n  # - { key: is_new,", 1)
	list = strings.Replace(list, "new_entries: { is_new: true }\n", "", 1) // it presets the detail that is gone
	if code, body, _ := s.post(l, base+"/settings/lists/work_type", url.Values{"source": {list}}, nil); code != http.StatusOK {
		t.Fatalf("change the list = %d: %s", code, problem(body))
	}

	_, page, _ := s.get(l, proc)
	for _, want := range []string{"Template version 0.1 is available", "Work types → New work type (needs owner approval)",
		`value="template_lists">Add it and use version 0.1`, `href="` + base + `/settings/lists/work_type">See Work types`} {
		if !strings.Contains(page, want) {
			t.Fatalf("process page is missing %q:\n%s", want, problem(page))
		}
	}
	if strings.Contains(page, `value="template">Use template version`) {
		t.Error("the plain template button shows although it can't work yet")
	}

	code, page, _ := s.post(l, proc, url.Values{"action": {"template_lists"}}, nil)
	if code != http.StatusOK || !strings.Contains(page, "version 0.1") || strings.Contains(page, "is available") {
		t.Fatalf("add and use = %d: %s", code, problem(page))
	}
	_, page, _ = s.get(l, base+"/settings/lists/work_type")
	if !strings.Contains(page, "<b>Region</b>") || !strings.Contains(page, "<b>New work type (needs owner approval)</b>") {
		t.Errorf("the list should keep Region and get the missing detail back:\n%s", problem(page))
	}
}
