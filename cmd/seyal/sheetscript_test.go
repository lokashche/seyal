// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"seyal/internal/playbook"
	"seyal/internal/send"
)

// TestSheetScriptSample keeps the sample Google Sheet script (examples/other-apps/sheet-script.gs,
// part 4c-4) safe and in step with Seyal: it must keep its safety marks, and its header row must
// have one column for each value the demo's "do: send" puts in a row. The script itself runs only
// in Google; it can't run here.
func TestSheetScriptSample(t *testing.T) {
	b, err := os.ReadFile("../../examples/other-apps/sheet-script.gs")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.HasPrefix(src, "/** @OnlyCurrentDoc */") {
		t.Error("the script must start with /** @OnlyCurrentDoc */, so it can touch only its own sheet")
	}
	if strings.Contains(src, "'"+strings.SplitAfter(send.TestNumber, "-")[0]+"'") {
		t.Error(`the script must tell a test send by the signed "test": true, never by its number: items may be numbered TEST-...`)
	}
	if regexp.MustCompile(`function\s+doGet\b`).MatchString(src) {
		t.Error("the script must have no doGet: nobody may read the sheet through its address")
	}
	for _, mark := range []string{
		"function doPost(", "LockService.getScriptLock()", "computeHmacSha256Signature", "'SEYAL_SECRET'",
		`',"sig":'`, "MAX_AGE_SECONDS = 5 * 60", "'Seyal test'", "signed.body.test === true",
		"'=+-@\\t\\r'", "ContentService.MimeType.JSON",
	} {
		if !strings.Contains(src, mark) {
			t.Errorf("the script lost %s", mark)
		}
	}

	// The header row: one column for the number, one per sent field, the currency once after the
	// first money field, then the state and the send id (internal/workitem/outbox.go, sendBody).
	m := regexp.MustCompile(`var HEADERS = \[([^\]]*)\];`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the script has no HEADERS line")
	}
	headers := strings.Count(m[1], "'") / 2
	p, err := playbook.LoadFile("../../examples/seed/job-evaluation-send.yaml")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, f := range p.Fields {
		types[f.Key] = f.Type
	}
	sends := 0
	for _, actions := range p.On {
		for _, a := range actions {
			if a.Do != "send" {
				continue
			}
			sends++
			want, money := 1+len(a.Fields)+2, false
			for _, f := range a.Fields {
				if types[f] == "money" && !money {
					want, money = want+1, true
				}
			}
			if headers != want {
				t.Errorf("the script's header row has %d columns; the demo's send makes rows of %d", headers, want)
			}
		}
	}
	if sends != 1 {
		t.Errorf("the demo process has %d send lines; want 1", sends)
	}
}

// TestSheetScriptRuns runs the sample sheet script under Node (when it is installed), with small
// stand-ins for Google's services (testdata/sheetscript.js), on posts signed by Seyal's own code
// (send.Signed with the signature in the body, as for script.google.com). It checks that a real
// send adds a row and a repeat updates it (also for numbers like 0001 or 2026-01 that a sheet would
// read as a number or a date, and +0001 that Seyal's guard gives a ' in front), a real item numbered
// TEST-... still goes to the real tab, a late retry of an older send doesn't overwrite a newer one, every
// write is saved before the lock is let go, a test send goes to the test tab, a formula stays text,
// and that a wrong or badly pasted secret, an old post, changed data, text after the signature, a
// missing tab, a changed header row and a busy sheet each get their plain answer. Google itself is
// never called.
func TestSheetScriptRuns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, so the sample sheet script can't be run here")
	}
	secret, err := send.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	other, err := send.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	formula := `=HYPERLINK("x")`
	real := map[string]any{"type": "process-one.approved", "id": "snd_acme-demo_1", "timestamp": now.UTC().Format(time.RFC3339),
		"business": "acme-demo", "data": map[string]any{"number": "J-2026-0007", "process": "process-one", "state": "approved",
			"fields": map[string]any{"name": map[string]any{"key": "acme", "name": formula},
				"amount": map[string]any{"minor": 1500000, "currency": "INR", "text": "₹15,000.00"}},
			"row": []any{"J-2026-0007", formula, "Work type ₹", 15000, "INR", 5000, 21, "approved", "snd_acme-demo_1"}}}
	guarded, err := send.GuardAll(real)
	if err != nil {
		t.Fatal(err)
	}
	realBody, _ := json.Marshal(guarded)
	unguarded, _ := json.Marshal(real) // as if Seyal's own guard had missed it: the script must catch it
	test, err := send.TestBody(send.Target{Fields: []string{"name"}}, "acme-demo", "INR", "snd_acme-demo_test_1", now)
	if err != nil {
		t.Fatal(err)
	}
	// another makes a guarded body for item number with send id, as Seyal's sendBody would.
	another := func(id, number string, fee int) []byte {
		b, err := send.GuardAll(map[string]any{"type": "process-one.approved", "id": id, "timestamp": now.UTC().Format(time.RFC3339),
			"business": "acme-demo", "data": map[string]any{"number": number, "process": "process-one", "state": "approved",
				"row": []any{number, "Acme Traders", "Work type", fee, "INR", 0, 21, "approved", id}}})
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(b)
		return out
	}
	sign := func(secret, id string, at time.Time, body []byte) string {
		_, out, err := send.Signed(secret, id, at, body, true)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	posts, _ := json.Marshal(map[string]any{"secret": secret, "posts": map[string]string{
		"real":        sign(secret, "snd_acme-demo_1", now, realBody),
		"unguarded":   sign(secret, "snd_acme-demo_1", now, unguarded),
		"test":        sign(secret, "snd_acme-demo_test_1", now, test),
		"old":         sign(secret, "snd_acme-demo_1", now.Add(-10*time.Minute), realBody),
		"othersecret": sign(other, "snd_acme-demo_1", now, realBody),
		"empty":       sign(secret, "snd_acme-demo_2", now, []byte("{}")),
		"zeros":       sign(secret, "snd_acme-demo_3", now, another("snd_acme-demo_3", "0001", 15000)),
		"datey":       sign(secret, "snd_acme-demo_4", now, another("snd_acme-demo_4", "2026-01", 15000)),
		"newer":       sign(secret, "snd_acme-demo_2", now, another("snd_acme-demo_2", "J-2026-0007", 18000)),
		"plus":        sign(secret, "snd_acme-demo_5", now, another("snd_acme-demo_5", "+0001", 15000)),
		"testnumber":  sign(secret, "snd_acme-demo_6", now, another("snd_acme-demo_6", "TEST-2026-0001", 15000)),
	}})
	path := filepath.Join(t.TempDir(), "posts.json")
	if err := os.WriteFile(path, posts, 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../examples/other-apps/sheet-script.gs")
	if err != nil {
		t.Fatal(err)
	}
	// Go's test cache only notices files the test itself opens, not those Node opens: read both, so
	// a change to either one runs this test again.
	for _, f := range []string{script, "testdata/sheetscript.js"} {
		if _, err := os.ReadFile(f); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(node, "testdata/sheetscript.js", script, path).CombinedOutput()
	t.Logf("node:\n%s", out)
	if err != nil {
		t.Fatalf("the sample sheet script failed a case: %v", err)
	}
	if !strings.Contains(string(out), "ALL PASS") {
		t.Fatal("the sheet script check didn't finish")
	}
}
