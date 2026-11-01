//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"bytes"
	"html"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// sendSheet posts a CSV file the way the browser's file input does.
func (s *site) sendSheet(c *http.Client, path, content string) (int, string) {
	s.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("sheet", "sheet.csv")
	fw.Write([]byte(content))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, html.UnescapeString(out.String())
}

// TestImportList: a spreadsheet is checked row by row first; nothing is saved while a row has a
// problem; confirming saves new and changed rows; the download reads back in as "nothing to do".
func TestImportList(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug + "/settings/master/work_type"
	l := s.logIn(lakshmi)

	bad := "\ufeffName,Standard fee,Default maker,Documents required,Is new,Notes\r\n" +
		"Monthly bookkeeping,15k,Priya Raman,Bank statements; Sales register,,x\r\n" +
		"Payroll,eight thousand,Karthik Subramanian,Attendance; Salary sheet,yes,\r\n" +
		"payroll,8000,,,,\r\n"
	_, page := s.sendSheet(l, base+"/import", bad)
	for _, want := range []string{"2 with problems", "Standard fee:", "Also on row 3", "Nothing is saved while a row has a problem", "“Notes”"} {
		if !strings.Contains(page, want) {
			t.Errorf("the check of a bad file is missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `name="action" value="apply"`) {
		t.Error("a file with problems can be imported")
	}

	good := "Name;Standard fee;Default maker;Documents required;Is new\n" + // ";" as Excel writes in many countries
		"Monthly bookkeeping;15k;Priya Raman;Bank statements; Sales register;\n"
	good = strings.Replace(good, "Bank statements; Sales register", `"Bank statements; Sales register"`, 1) +
		"Payroll;8000;Karthik Subramanian;\"Attendance; Salary sheet\";yes\n"
	_, page = s.sendSheet(l, base+"/import", good)
	if !strings.Contains(page, "1 new · 1 changed") || !strings.Contains(page, "Import 2 rows") ||
		!strings.Contains(page, "Standard fee, Default maker, Documents required") {
		t.Fatalf("the check of a good file:\n%s", page)
	}
	_, sheet, _ := s.get(l, base+"/export")
	if strings.Contains(sheet, "Payroll") {
		t.Fatal("checking a file saved it")
	}

	_, page, _ = s.post(l, base+"/import", url.Values{"action": {"apply"}, "csv": {good}}, nil)
	if !strings.Contains(page, "Imported: 1 new, 1 changed, 0 unchanged.") {
		t.Fatalf("import:\n%s", problem(page))
	}
	_, sheet, h := s.get(l, base+"/export")
	if !strings.HasPrefix(h.Get("Content-Type"), "text/csv") || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Errorf("the download isn't a CSV file: %v", h)
	}
	for _, want := range []string{"Payroll,Attendance; Salary sheet,,Karthik Subramanian,,,8000,,yes,",
		"Monthly bookkeeping,", "Bank statements; Sales register", ",15000,"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("the download is missing %q:\n%s", want, sheet)
		}
	}

	// The download, imported as it is, changes nothing.
	_, page = s.sendSheet(l, base+"/import", sheet)
	if !strings.Contains(page, "Everything in the file is already saved") || strings.Contains(page, "with problems") {
		t.Errorf("re-importing the download:\n%s", page)
	}
}

// TestImportPeople: people are matched by email; new people get a password shown once (and it
// works); roles are added by name; unknown roles and bad emails stop the import; only admins import.
func TestImportPeople(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug + "/settings/people"
	l := s.logIn(lakshmi)

	_, page := s.sendSheet(l, base+"/import", "Name,Email,Roles\nMeena Pillai,meena@kaveri-demo.example,Wizard\nNo Mail,,\n")
	if !strings.Contains(page, `No role called "Wizard"`) || !strings.Contains(page, "No valid email") {
		t.Errorf("bad rows should be shown:\n%s", page)
	}

	good := "Name,Email,Roles\n" +
		"Meena Pillai,MEENA@kaveri-demo.example,Operations; sales\n" +
		"Karthik Subramanian,karthik@kaveri-demo.example,Operations\n" +
		"Priya Raman,priya@kaveri-demo.example,ops\n"
	_, page = s.sendSheet(l, base+"/import", good)
	if !strings.Contains(page, "1 new · 1 changed · 1 unchanged") || !strings.Contains(page, "Gets Operations") {
		t.Fatalf("the check:\n%s", page)
	}
	_, page, _ = s.post(l, base+"/import", url.Values{"action": {"apply"}, "csv": {good}}, nil)
	page = html.UnescapeString(page)
	m := regexp.MustCompile(`Meena Pillai: ([A-Za-z0-9]+)`).FindStringSubmatch(page)
	if !strings.Contains(page, "Imported: 1 new, 1 changed, 1 unchanged.") || m == nil {
		t.Fatalf("import:\n%s", page)
	}
	_, roles, _ := s.get(l, "/t/"+s.slug+"/settings/roles")
	if !strings.Contains(roles, "Karthik Subramanian, Meena Pillai, Priya Raman") {
		t.Errorf("Operations should now be Karthik, Meena and Priya:\n%s", roles)
	}
	s.passwords["meena@kaveri-demo.example"] = m[1]
	s.logIn("meena@kaveri-demo.example") // fails the test if the password doesn't work

	if code, _ := s.sendSheet(s.logIn(arjun), base+"/import", good); code != http.StatusForbidden {
		t.Errorf("Arjun (not an admin) importing = %d, want 403", code)
	}
}
