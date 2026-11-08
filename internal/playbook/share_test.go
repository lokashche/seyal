// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"strings"
	"testing"
)

// shareExample is timingExample with share messages (D91): one for a colleague, one for outside.
const shareExample = timingExample + `share:
  - id: ask-papers
    label: Ask for the papers
    to: outside
    in: [with_team]
    text: "Dear {topic}, please send the papers for {number} by {due}. Thank you, {me}, {business}"
  - id: heads_up
    label: Heads-up for a colleague
    to: colleague
    text: "{number} ({title}) needs a look: {link}"
`

func TestShareMessages(t *testing.T) {
	p, err := Parse([]byte(shareExample))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckNew(); err != nil {
		t.Fatalf("a good share list is refused: %v", err)
	}
	if !p.Share[0].Offered("with_team") || p.Share[0].Offered("checking") || !p.Share[1].Offered("checking") {
		t.Error("in: offers a message in the wrong steps")
	}

	for name, c := range map[string]struct{ from, to, want string }{
		"an outside message with the link": {`please send the papers for {number}`, `please send the papers at {link}`,
			"share ask-papers: an outside message can't carry {link}"},
		"a field called due": {`- { key: topic, label: Topic, type: text, required: true }`,
			"- { key: topic, label: Topic, type: text, required: true }\n  - { key: due, label: Due, type: date }",
			`field due: a process with share messages can't have a field called "due"`},
		"an unknown placeholder": {`{business}"`, `{firm}"`, "share ask-papers: text: unknown placeholder {firm}"},
		"an unknown step":        {`in: [with_team]`, `in: [with_teem]`, `share ask-papers: in: unknown state "with_teem"`},
		"a wrong to":             {`to: colleague`, `to: everyone`, `share heads_up: to "everyone": use colleague or outside`},
		"Seyal's own id":         {`id: heads_up`, `id: colleague`, `share colleague: "colleague" is Seyal's own "Tell a colleague"`},
		"a repeated id":          {`id: heads_up`, `id: ask-papers`, "share ask-papers: duplicate id"},
		"a bad id":               {`id: heads_up`, `id: Heads Up`, `id "Heads Up" must start with a lowercase letter`},
		"a long label":           {`label: Ask for the papers`, `label: ` + strings.Repeat("x", 41), "share ask-papers: label must be 1 to 40 letters"},
		"a long text": {`text: "{number} ({title}) needs a look: {link}"`, `text: "` + strings.Repeat("x", 1501) + `"`,
			"share heads_up: text must be 1 to 1500 letters"},
	} {
		if !strings.Contains(shareExample, c.from) {
			t.Fatalf("%s: the example has no %q", name, c.from)
		}
		p, err := Parse([]byte(strings.Replace(shareExample, c.from, c.to, 1)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := p.CheckNew(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: CheckNew = %v; want %q", name, err, c.want)
		}
	}

	// A field called due is fine in a process without share messages.
	plain := strings.Replace(timingExample, "- { key: topic, label: Topic, type: text, required: true }",
		"- { key: topic, label: Topic, type: text, required: true }\n  - { key: due, label: Due, type: date }", 1)
	if p, err := Parse([]byte(plain)); err != nil || p.CheckNew() != nil {
		t.Errorf("a field called due without share messages: %v", err)
	}
}
