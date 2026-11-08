// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"slices"
	"strings"
	"testing"

	"seyal/internal/setup"
)

// TestSendSwitch: sending to other apps is off unless SEYAL_SEND says "on" (D95); a typo, or a host
// list with an address or a port in it, stops the start.
func TestSendSwitch(t *testing.T) {
	env := func(on, hosts string) func(string) string {
		return func(k string) string {
			return map[string]string{"SEYAL_SEND": on, "SEYAL_SEND_HOSTS": hosts}[k]
		}
	}
	for on, want := range map[string]bool{"": false, "off": false, "on": true} {
		got, hosts, err := sending(env(on, "script.google.com, Hook.EU1.make.com"))
		if got != want || err != nil || !hosts.Allows("hook.eu1.make.com") || hosts.Allows("example.com") {
			t.Errorf("SEYAL_SEND=%q = %v, %v, %v; want %v", on, got, hosts, err, want)
		}
	}
	for _, bad := range [][2]string{{"yes", ""}, {"ON", ""}, {"on", "https://script.google.com"}, {"off", "hook.example:8443"}, {"on", "127.0.0.1"}} {
		if got, _, err := sending(env(bad[0], bad[1])); got || err == nil {
			t.Errorf("SEYAL_SEND=%q, SEYAL_SEND_HOSTS=%q = %v, %v; want an error and off", bad[0], bad[1], got, err)
		}
	}
}

// TestImportArgs: "--new-secret <name>" may come before or after the file, more than once.
func TestImportArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		path    string
		secrets []string
	}{
		{[]string{"firm.yaml"}, "firm.yaml", nil},
		{[]string{"firm.yaml", "--new-secret", "jobs-sheet"}, "firm.yaml", []string{"jobs-sheet"}},
		{[]string{"--new-secret=a", "firm.yaml", "--new-secret", "b"}, "firm.yaml", []string{"a", "b"}},
	} {
		path, secrets, err := importArgs(tc.args)
		if err != nil || path != tc.path || !slices.Equal(secrets, tc.secrets) {
			t.Errorf("%v = %q %v %v", tc.args, path, secrets, err)
		}
	}
	for _, bad := range [][]string{nil, {"a.yaml", "b.yaml"}, {"a.yaml", "--new-secret"}, {"a.yaml", "--new-secret", "--x"}, {"-x", "a.yaml"}} {
		if _, _, err := importArgs(bad); err == nil {
			t.Errorf("%v: want the usage", bad)
		}
	}
}

// TestOtherAppsSaid: the import prints each new secret once, says which apps it removed (with their
// secrets), and says when nothing can go yet, naming a missing redirect host too.
func TestOtherAppsSaid(t *testing.T) {
	f := &setup.File{OtherApps: []setup.OtherApp{{Name: "jobs-sheet", URL: "https://script.google.com/macros/s/MARKER/exec",
		RedirectTo: []string{"script.googleusercontent.com"}}}}
	var out strings.Builder
	otherAppsSaid(&out, f, map[string]string{"jobs-sheet": "whsec_SECRET"}, []string{"old-hook"}, "off", "hook.eu1.make.com")
	got := out.String()
	for _, want := range []string{"Signing secret for other app jobs-sheet (shown only now", "  whsec_SECRET\n",
		"Other app old-hook is no longer in the file, so it was removed and its signing secret thrown away",
		"off on this server (SEYAL_SEND)", "script.google.com, script.googleusercontent.com isn't in SEYAL_SEND_HOSTS"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "whsec_SECRET") != 1 || strings.Contains(got, "MARKER") {
		t.Errorf("the secret must show once and the address never:\n%s", got)
	}
	out.Reset()
	otherAppsSaid(&out, f, nil, nil, "on", "script.google.com")
	if got := out.String(); !strings.Contains(got, "jobs-sheet: script.googleusercontent.com isn't in SEYAL_SEND_HOSTS") {
		t.Errorf("a missing redirect host isn't named: %q", got)
	}
	out.Reset()
	otherAppsSaid(&out, f, nil, nil, "on", "script.google.com, script.googleusercontent.com")
	if out.String() != "" {
		t.Errorf("a re-import with sending on and the hosts listed says %q, want nothing", out.String())
	}
}
