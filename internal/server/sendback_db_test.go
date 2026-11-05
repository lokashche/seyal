//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"seyal/internal/setup"
)

// TestSendBackOnScreen: with the shipped samples, Ben takes a job from the Operations team. Under Back
// to sales he reads where it goes ("Goes back to Arjun Mehta."), and sends it back with a reason.
// Arjun's card shows "From Ben: <reason>", and under Send to ops he reads that it goes back to Ben.
// While Ben has the job, Arjun (who started it) can no longer edit the Maker, who gets the job once it
// is approved (D68, M4 and choice 14).
func TestSendBackOnScreen(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	var err error
	if s.passwords["ben"], err = setup.NewPassword(context.Background(), s.app, s.mail("ben"), s.tenant); err != nil {
		t.Fatal(err)
	}
	a, b := s.logIn(arjun), s.logIn("ben")
	_, home, _ := s.get(a, base+"/")
	m := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Job evaluation"`).FindStringSubmatch(home)
	if m == nil {
		t.Fatalf("Tell Seyal doesn't offer Job evaluation:\n%s", problem(home))
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {m[1]}, "text": {"Monthly books for Sundaram"}}, nil)
	job := h.Get("Location")
	for _, answer := range []string{"sundaram", "monthly bookkeeping", "Books for the year", "pass"} {
		s.post(a, job+"/say", url.Values{"text": {answer}}, nil)
	}
	if _, page, _ := s.get(a, job); !strings.Contains(page, `aria-label="Edit Maker"`) {
		t.Errorf("Arjun has the draft, so he may edit the Maker:\n%s", problem(page))
	}
	s.post(a, job+"/say", url.Values{"text": {"send to ops"}}, nil)
	if code, body, _ := s.post(b, job+"/take", url.Values{"holder": {"team:ops"}}, nil); code != http.StatusSeeOther {
		t.Fatalf("Ben's Take = %d: %s", code, problem(body))
	}

	_, page, _ := s.get(a, job)
	if strings.Contains(page, `aria-label="Edit Maker"`) || !strings.Contains(page, `aria-label="Edit Reason for job"`) {
		t.Errorf("with Ben holding the job, Arjun may edit other details but not the Maker:\n%s", problem(page))
	}
	_, page, _ = s.get(b, job)
	if !strings.Contains(page, "Goes back to Arjun Mehta.") || !strings.Contains(page, `aria-label="Edit Maker"`) {
		t.Errorf("Ben's panel should say where Back to sales goes, and let him edit the Maker:\n%s", problem(page))
	}
	// The hint sits under the reason form, not squeezed into its row, and the button points at it, so a
	// screen reader reads it with the button.
	if !regexp.MustCompile(`</form>\s*<p class="why hint" id="hint-back_to_sales">Goes back to Arjun Mehta.</p>`).MatchString(page) ||
		!strings.Contains(page, `<button class="btn warn sm" aria-describedby="hint-back_to_sales">`) {
		t.Errorf("the hint under Back to sales should follow its form and be linked to its button:\n%s", problem(page))
	}
	page = html.UnescapeString(s.send(b, job+"/act", url.Values{"action": {"back_to_sales"}, "reason": {"the March bank statement is missing"}}))
	if !strings.Contains(page, "gave it back to Arjun Mehta") {
		t.Errorf("the thread should say Seyal gave it back to Arjun:\n%s", page)
	}

	if _, home, _ := s.get(a, base+"/?f=turn"); !strings.Contains(html.UnescapeString(home), "From Ben: the March bank statement is missing") {
		t.Errorf("Arjun's card should show who sent it back and why:\n%s", home)
	}
	if _, page, _ := s.get(a, job); !strings.Contains(page, "Goes back to Ben.") {
		t.Errorf("Arjun's panel should say Send to ops goes back to Ben:\n%s", problem(page))
	}
}

// TestOwnerFieldListsReceivers: in Other work the step belongs to whoever "Who does it" names, so its
// select lists only the people who may receive the work (people with a login), plus whoever it names
// now, and whoever has it (Arjun, who started it), who may put his own name in (D68, choice 14; D71
// question 2; D72 review fix).
func TestOwnerFieldListsReceivers(t *testing.T) {
	s := newSiteWith(t, "../../examples/playbooks")
	base := "/t/" + s.slug
	if code, page, _ := s.post(s.logIn(lakshmi), base+"/settings/processes", url.Values{"template": {"other-work"}}, nil); code != http.StatusOK {
		t.Fatalf("add Other work = %d: %s", code, problem(page))
	}
	a := s.logIn(arjun)
	_, home, _ := s.get(a, base+"/")
	pv := regexp.MustCompile(`value="([^"]+)" form="tell-form" data-name="Other work"`).FindStringSubmatch(home)
	if pv == nil {
		t.Fatalf("Tell Seyal doesn't offer Other work:\n%s", problem(home))
	}
	_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv[1]}, "text": {"Water the plants in reception"}}, nil)
	item := h.Get("Location")
	_, page, _ := s.get(a, item)
	i := strings.Index(page, `aria-label="Who does it"`)
	if i < 0 {
		t.Fatalf("the panel should let Arjun choose who does it:\n%s", problem(page))
	}
	options := page[i : i+strings.Index(page[i:], "</select>")]
	for _, want := range []string{"Priya Raman", "Lakshmi Narayanan", "Arjun Mehta"} {
		if !strings.Contains(options, want) {
			t.Errorf("Who does it should offer %s (has a login):\n%s", want, options)
		}
	}
	for _, not := range []string{"Karthik Subramanian", "Fatima Sheikh"} {
		if strings.Contains(options, not) {
			t.Errorf("Who does it offers %s, who has no login and so can't receive work:\n%s", not, options)
		}
	}
}
