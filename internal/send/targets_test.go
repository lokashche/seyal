// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func demoTarget() Target {
	return Target{Name: "jobs-sheet", Label: "Jobs sheet", Service: "Google",
		URL:    "https://script.google.com/macros/s/MARKER0000/exec",
		Fields: []string{"client", "fee_agreed"}, Secret: "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"}.Clean()
}

// TestTargetCheck: a set-up file's other app is checked, and no message holds the address.
func TestTargetCheck(t *testing.T) {
	if p := demoTarget().Check(); len(p) != 0 {
		t.Errorf("a good target: %v", p)
	}
	for _, tc := range []struct {
		name string
		edit func(*Target)
		says string
	}{
		{"bad name", func(t *Target) { t.Name = "Jobs Sheet" }, "a name is lowercase"},
		{"no label", func(t *Target) { t.Label = " " }, "label is required"},
		{"no service", func(t *Target) { t.Service = "" }, "service is required"},
		{"http", func(t *Target) { t.URL = "http://script.google.com/MARKER0000" }, "not https"},
		{"a port", func(t *Target) { t.URL = "https://script.google.com:8443/MARKER0000" }, "host not allowed"},
		{"an IP address", func(t *Target) { t.URL = "https://10.0.0.1/MARKER0000" }, "not an IP address"},
		{"redirect with a scheme", func(t *Target) { t.RedirectTo = []string{"https://x.example"} }, "redirect_to"},
		{"expect", func(t *Target) { t.Expect = "yes" }, `expect "yes"`},
		{"field twice", func(t *Target) { t.Fields = []string{"a", "a"} }, `"a" is not a field key, or is listed twice`},
		{"field key", func(t *Target) { t.Fields = []string{"Fee Agreed"} }, "is not a field key"},
	} {
		tg := demoTarget()
		tc.edit(&tg)
		p := strings.Join(tg.Check(), "; ")
		if !strings.Contains(p, tc.says) {
			t.Errorf("%s: problems %q, want %q", tc.name, p, tc.says)
		}
		if strings.Contains(p, "MARKER0000") {
			t.Errorf("%s: a problem shows the address: %q", tc.name, p)
		}
	}
}

// TestTargetSummary: what the ledger keeps about a target has its hash, never the address or secret.
func TestTargetSummary(t *testing.T) {
	tg := demoTarget()
	if tg.Expect != ExpectOKJSON || tg.Host() != "script.google.com" || tg.AddressEnd() != "…/exec" {
		t.Errorf("clean target: expect %q, host %q, ends %q", tg.Expect, tg.Host(), tg.AddressEnd())
	}
	b, err := json.Marshal(tg.Summary("new"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "MARKER0000") || strings.Contains(string(b), tg.Secret[6:]) || !strings.Contains(string(b), tg.Hash()) {
		t.Errorf("summary %s", b)
	}
	// Every column changes the hash.
	for i, edit := range []func(*Target){
		func(t *Target) { t.Label = "x" }, func(t *Target) { t.Service = "x" }, func(t *Target) { t.URL += "x" },
		func(t *Target) { t.RedirectTo = []string{"x.example"} }, func(t *Target) { t.Expect = Expect2xx },
		func(t *Target) { t.Fields = t.Fields[:1] }, func(t *Target) { t.Link = true },
		func(t *Target) { t.Secret = "whsec_other" }, func(t *Target) { t.Paused = true },
	} {
		other := demoTarget()
		edit(&other)
		if other.Hash() == tg.Hash() {
			t.Errorf("edit %d didn't change the hash", i)
		}
	}
	if (Target{Fields: nil}).Hash() != (Target{Fields: []string{}}).Hash() {
		t.Error("an empty list and no list hash differently (the database gives back empty lists)")
	}
}

// TestAddressEnd: the end of an address is shown (and kept in the ledger) only when it is a fixed word
// like Apps Script's "/exec". Services that end their address in a code (Zapier, Make, n8n) show none
// of it.
func TestAddressEnd(t *testing.T) {
	for address, want := range map[string]string{
		"https://script.google.com/macros/s/MARKER0000/exec":           "…/exec",
		"https://script.google.com/macros/s/MARKER0000/dev":            "…/dev",
		"https://hooks.zapier.com/hooks/catch/000000/MARKER0/":         "",
		"https://hook.eu1.make.com/MARKER0000abcdefghijklmnopqrstuv":   "",
		"https://n8n.acme.example/webhook/seyal":                       "",
		"https://n8n.acme.example/webhook/0000-MARKER0000":             "",
		"https://script.google.com/macros/s/MARKER0000/exec?key=MARK0": "",
		"https://script.google.com/macros/s/MARKER0000/execMARK0":      "",
	} {
		tg := Target{URL: address}
		if got := tg.AddressEnd(); got != want {
			t.Errorf("AddressEnd(%s) = %q, want %q", address, got, want)
		}
		b, _ := json.Marshal(tg.Summary("new"))
		if strings.Contains(string(b), "MARK") || strings.Contains(string(b), "seyal\"") {
			t.Errorf("summary of %s holds part of its code: %s", address, b)
		}
	}
}

// TestCleanScheme: "HTTPS://" is stored as "https://", as the table's check wants (an import must not
// fail at the database after the file was accepted).
func TestCleanScheme(t *testing.T) {
	tg := Target{Name: "a", Label: "A", Service: "S", URL: " HTTPS://script.google.com/macros/s/X/exec "}.Clean()
	if tg.URL != "https://script.google.com/macros/s/X/exec" || len(tg.Check()) != 0 {
		t.Errorf("cleaned %q, problems %v", tg.URL, tg.Check())
	}
}

// TestHostsOff: a target's own host and each host it redirects to must be on the server's list.
func TestHostsOff(t *testing.T) {
	tg := Target{URL: "https://script.google.com/x/exec", RedirectTo: []string{"script.googleusercontent.com"}}.Clean()
	hosts, _ := ParseHosts("script.google.com")
	if got := strings.Join(tg.HostsOff(hosts), ","); got != "script.googleusercontent.com" {
		t.Errorf("redirect host not listed: off %q", got)
	}
	hosts, _ = ParseHosts("script.google.com, script.googleusercontent.com")
	if off := tg.HostsOff(hosts); len(off) != 0 {
		t.Errorf("both listed: off %v", off)
	}
	if got := strings.Join(tg.HostsOff(nil), ","); got != "script.google.com,script.googleusercontent.com" {
		t.Errorf("nothing listed: off %q", got)
	}
}

type fakePoster struct {
	address string
	header  http.Header
	body    []byte
	answer  Answer
	err     error
}

func (f *fakePoster) PostRedirect(_ context.Context, address string, _ []string, header http.Header, body []byte) (Answer, error) {
	f.address, f.header, f.body = address, header, body
	return f.answer, f.err
}

// TestSendATest: a test carries only fictional values, signed with the target's secret (inside the
// body too, for Apps Script), and the answer is judged by the target's expectation.
func TestSendATest(t *testing.T) {
	tg := demoTarget()
	at := time.Date(2026, 11, 8, 8, 32, 10, 0, time.UTC)
	id := TestID("kaveri-demo", at)
	if err := checkID(id); err != nil || !strings.HasPrefix(id, "snd_kaveri-demo_test_") {
		t.Fatalf("test id %q: %v", id, err)
	}
	body, err := TestBody(tg, "kaveri-demo", "INR", id, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"business":"kaveri-demo","data":{"fields":{"client":"test","fee_agreed":"test"},"number":"TEST-0000",` +
		`"process":"test","row":["TEST-0000","Acme Traders",0,"INR","test","` + id + `"],"state":"test"},` +
		`"id":"` + id + `","test":true,"timestamp":"2026-11-08T08:32:10Z","type":"seyal.test"}`
	if string(body) != want {
		t.Errorf("test body\n got %s\nwant %s", body, want)
	}

	f := &fakePoster{answer: Answer{Status: 200, Body: []byte(`{"ok":true,"row":2}`)}}
	if v := Test(context.Background(), f, tg, id, at, body); v.Kind != Done || v.Reason != ReasonOK {
		t.Errorf("verdict %+v", v)
	}
	if f.address != tg.URL || f.header.Get("webhook-id") != id || f.header.Get("webhook-timestamp") != strconv.FormatInt(at.Unix(), 10) {
		t.Errorf("posted to the wrong place or with the wrong headers: %v", f.header)
	}
	if sig, _ := Sign(tg.Secret, id, at, f.body); f.header.Get("webhook-signature") != sig {
		t.Error("the header signature isn't over the bytes posted")
	}
	if !strings.Contains(string(f.body), `,"sig":{"id":"`+id+`"`) {
		t.Errorf("an Apps Script target gets the signature inside the body too: %s", f.body)
	}

	// ok_json: 200 without "ok" is tried again; a 2xx target takes any 2xx.
	f.answer = Answer{Status: 200, Body: []byte("<html>")}
	if v := Test(context.Background(), f, tg, id, at, body); v.Kind != Retry {
		t.Errorf("ok_json without ok: %+v", v)
	}
	plain := tg
	plain.URL, plain.Expect = "https://hook.eu1.make.example/abc", Expect2xx
	if v := Test(context.Background(), f, plain, id, at, body); v.Kind != Done {
		t.Errorf("2xx target: %+v", v)
	}
	if strings.Contains(string(f.body), `"sig"`) {
		t.Error("a target that reads headers got a signature in the body")
	}
	// A refused call (a host not on the list) is "an admin must fix it", with a fixed reason.
	f.err = fail(ReasonHost)
	if v := Test(context.Background(), f, plain, id, at, body); v.Kind != Fix || v.Reason != ReasonHost {
		t.Errorf("refused host: %+v", v)
	}
}

// TestHostNotListedRefused: the real caller for other apps never calls a host the server's list
// doesn't have, before any connection is made.
func TestHostNotListedRefused(t *testing.T) {
	hosts, err := ParseHosts("hook.eu1.make.com")
	if err != nil {
		t.Fatal(err)
	}
	tg := demoTarget() // script.google.com, not on the list
	body, _ := TestBody(tg, "kaveri-demo", "INR", "snd_x", time.Now())
	v := Test(context.Background(), NewAppCaller(hosts), tg, "snd_x", time.Now(), body)
	if v.Kind != Fix || v.Reason != ReasonHost {
		t.Errorf("verdict %+v, want %q", v, ReasonHost)
	}
}
