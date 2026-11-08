// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

// sendSample is the demo-only copy of Job evaluation that sends approved jobs to an other app (D96).
const sendSample = "../../examples/seed/job-evaluation-send.yaml"

// TestSendAction: `do: send` in a process (D96) names an other app and the fields that go; anything
// else is refused with a clear problem. The demo-only sample passes every check, CheckNew too.
func TestSendAction(t *testing.T) {
	p, err := LoadFile(sendSample)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("the sample: %v", err)
	}
	if err := p.CheckNew(); err != nil {
		t.Fatalf("the sample, as a new version: %v", err)
	}
	var found bool
	for _, a := range p.On["approved"] {
		if a.Do == "send" && a.To == "jobs-sheet" && len(a.Fields) == 5 && !a.Link {
			found = true
		}
	}
	if !found {
		t.Fatalf("the sample's approved step has no send to jobs-sheet with five fields: %+v", p.On["approved"])
	}

	source := string(p.Source)
	const send = "    - do: send\n      to: jobs-sheet\n      fields: [client, nature_of_work, fee_agreed, advance, timeline_days]\n"
	if !strings.Contains(source, send) {
		t.Fatal("test setup: the sample's send lines changed; update this test")
	}
	for _, tt := range []struct{ name, new, want string }{
		{"no app", "    - do: send\n      fields: [client]\n", `send needs "to"`},
		{"an app name with capitals", "    - do: send\n      to: Jobs-Sheet\n      fields: [client]\n", `send needs "to"`},
		{"no fields", "    - do: send\n      to: jobs-sheet\n", "send needs fields"},
		{"an unknown field", "    - do: send\n      to: jobs-sheet\n      fields: [client, colour]\n", `unknown field "colour"`},
		{"a field twice", "    - do: send\n      to: jobs-sheet\n      fields: [client, client]\n", "send lists client twice"},
		{"a list entry's detail", "    - do: send\n      to: jobs-sheet\n      fields: [client.is_new]\n", "send the process's own fields only"},
		{"text on a send", "    - do: send\n      to: jobs-sheet\n      text: hello\n      fields: [client]\n", `send needs "to"`},
		{"fields on a post", "    - { do: post, text: Hi, fields: [client] }\n    - do: send\n      to: jobs-sheet\n      fields: [client]\n",
			"fields and link only go with send"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed, err := Parse([]byte(strings.Replace(source, send, tt.new, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if err := changed.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v\nwant a problem mentioning %q", err, tt.want)
			}
		})
	}
}
