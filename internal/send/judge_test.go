// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"errors"
	"strings"
	"testing"
)

func TestJudge(t *testing.T) {
	long := strings.Repeat("é", 150)
	for name, c := range map[string]struct {
		a      Answer
		err    error
		okJSON bool
		want   Verdict
	}{
		"200":                      {Answer{Status: 200}, nil, false, Verdict{Done, "answered OK"}},
		"204":                      {Answer{Status: 204}, nil, false, Verdict{Done, "answered OK"}},
		"ok true":                  {Answer{Status: 200, Body: []byte(`{"ok":true,"row":12}`)}, nil, true, Verdict{Done, "answered OK"}},
		"ok false":                 {Answer{Status: 200, Body: []byte(`{"ok":false,"why":"no tab called Jobs"}`)}, nil, true, Verdict{Fix, "script failed: no tab called Jobs"}},
		"ok false, no why":         {Answer{Status: 200, Body: []byte(`{"ok":false}`)}, nil, true, Verdict{Fix, "script failed: no reason"}},
		"ok false, messy why":      {Answer{Status: 200, Body: []byte(`{"ok":false,"why":"line one\r\n\tline\u0000two"}`)}, nil, true, Verdict{Fix, "script failed: line one line two"}},
		"ok false, hidden marks":   {Answer{Status: 200, Body: []byte(`{"ok":false,"why":"no tab ​called ‮sboj⁦x⁩‏"}`)}, nil, true, Verdict{Fix, "script failed: no tab called sboj x"}},
		"ok false, long why":       {Answer{Status: 200, Body: []byte(`{"ok":false,"why":"` + long + `"}`)}, nil, true, Verdict{Fix, "script failed: " + long[:200]}},
		"ok_json, HTML page":       {Answer{Status: 200, Body: []byte(`<html>Error</html>`)}, nil, true, Verdict{Retry, "answered without an OK"}},
		"ok_json, no ok":           {Answer{Status: 200, Body: []byte(`{"row":12}`)}, nil, true, Verdict{Retry, "answered without an OK"}},
		"302 to sign-in":           {Answer{Status: 302, RedirectHost: "accounts.google.com"}, nil, true, Verdict{Fix, "needs sign-in"}},
		"302 elsewhere":            {Answer{Status: 302, RedirectHost: "other.example"}, nil, true, Verdict{Fix, "answered 302"}},
		"400":                      {Answer{Status: 400}, nil, false, Verdict{Fix, "answered 400"}},
		"404":                      {Answer{Status: 404}, nil, false, Verdict{Fix, "answered 404"}},
		"410":                      {Answer{Status: 410}, nil, false, Verdict{Gone, "answered 410"}},
		"429":                      {Answer{Status: 429}, nil, false, Verdict{Retry, "answered 429"}},
		"503":                      {Answer{Status: 503}, nil, false, Verdict{Retry, "answered 503"}},
		"timeout":                  {Answer{}, fail(ReasonTimeout), false, Verdict{Retry, "timeout"}},
		"refused":                  {Answer{}, fail(ReasonConnect), false, Verdict{Retry, "could not connect"}},
		"TLS":                      {Answer{}, fail(ReasonTLS), false, Verdict{Retry, "TLS error"}},
		"blocked":                  {Answer{}, fail(ReasonBlocked), false, Verdict{Fix, "address not allowed"}},
		"host not allowed":         {Answer{}, fail(ReasonHost), false, Verdict{Fix, "host not allowed"}},
		"an error of another kind": {Answer{}, errors.New("Post https://secret.example/x: boom"), false, Verdict{Fix, "failed"}},
	} {
		if got := Judge(c.a, c.err, c.okJSON); got != c.want {
			t.Errorf("%s: Judge = %+v, want %+v", name, got, c.want)
		}
	}
}

func TestParseHosts(t *testing.T) {
	h, err := ParseHosts(" Script.Google.com, script.googleusercontent.com,,hook.eu1.make.com ")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"script.google.com", "script.googleusercontent.com", "hook.eu1.make.com"} {
		if !h.Allows(host) {
			t.Errorf("%s not allowed", host)
		}
	}
	if h.Allows("hooks.zapier.com") || h.Allows("google.com") {
		t.Error("a host not on the list is allowed")
	}
	if h, err := ParseHosts(""); err != nil || len(h) != 0 {
		t.Errorf("an empty list = %v, %v; want nothing allowed", h, err)
	}
	for _, bad := range []string{"https://hook.example.com", "hook.example.com:8443", "localhost", "10.0.0.5",
		"hook.example.com/path", ".example.com", "example.com.", "[::1]", "hook example.com"} {
		if _, err := ParseHosts(bad); err == nil {
			t.Errorf("ParseHosts(%q) took it", bad)
		}
	}
}

// A whole webhook address pasted into the list must not come back in the error (its path is often a
// secret, and the error goes to the server log and the import output).
func TestParseHostsHidesTheEntry(t *testing.T) {
	_, err := ParseHosts("script.google.com, https://hook.eu1.make.com/markerword123")
	if err == nil {
		t.Fatal("a full address was taken")
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "markerword123") || strings.Contains(msg, "https://hook") {
		t.Errorf("the error repeats the address: %s", msg)
	}
	if !strings.Contains(msg, "entry 2") || !strings.Contains(msg, "hook.eu1.make.com") {
		t.Errorf("the error should name entry 2 and its host only: %s", msg)
	}
	_, err = ParseHosts("hook.example.com/markerword123")
	if err == nil || strings.Contains(err.Error(), "markerword123") {
		t.Errorf("a host with a path: %v", err)
	}
}
