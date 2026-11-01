//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// TestReports: a filled job prints as a report (details, approvals of this version with who and
// when, what is still needed, its place in the ledger); an admin downloads all jobs as a spreadsheet.
func TestReports(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"Books for Sundaram"}}, nil)
	item := h.Get("Location")
	for _, answer := range []string{"Sundaram Textiles", "Monthly bookkeeping", "Books for the year", "10k", "pass"} {
		s.send(a, item+"/say", url.Values{"text": {answer}})
	}
	s.send(a, item+"/act", url.Values{"action": {"submit"}})
	s.send(a, item+"/decide", url.Values{"slot": {"sales"}, "decision": {"approve"}})

	_, page, _ := s.get(a, item)
	if !strings.Contains(page, item+`/report"`) {
		t.Fatal("the item should link to its report")
	}
	code, report, _ := s.get(a, item+"/report")
	report = html.UnescapeString(report)
	for _, want := range []string{"Kaveri Associates (demo)", "Sundaram Textiles — Monthly bookkeeping", "Fee agreed", "₹10,000",
		"<td>approved</td><td>Arjun Mehta</td>", "Still needed:", "Ledger check: event", "Details fingerprint:", "data-print"} {
		if !strings.Contains(report, want) {
			t.Errorf("report (%d) is missing %q", code, want)
		}
	}

	if code, _, _ := s.get(a, base+"/export/job-evaluation"); code != http.StatusForbidden {
		t.Errorf("Arjun (not an admin) downloading all jobs = %d, want 403", code)
	}
	l := s.logIn(lakshmi)
	if _, list, _ := s.get(l, base+"/?p=job-evaluation"); !strings.Contains(list, `href="`+base+`/export/job-evaluation"`) {
		t.Error("an admin filtering by a process should see the download")
	}
	code, sheet, hd := s.get(l, base+"/export/job-evaluation")
	if code != http.StatusOK || !strings.HasPrefix(hd.Get("Content-Type"), "text/csv") {
		t.Fatalf("download = %d %q", code, hd.Get("Content-Type"))
	}
	for _, want := range []string{"Number,Title,Status,With,Started by,Started on,Last changed,Due", "Fee agreed",
		"Sundaram Textiles — Monthly bookkeeping,pending approval", ",Arjun Mehta,", ",10000,"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("spreadsheet is missing %q:\n%s", want, sheet)
		}
	}
	if code, _, _ := s.get(l, base+"/export/no-such-process"); code != http.StatusNotFound {
		t.Errorf("unknown process = %d, want 404", code)
	}
}
