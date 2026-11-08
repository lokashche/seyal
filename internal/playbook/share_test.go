// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"reflect"
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
		"a contact that isn't a list": {`to: outside`, "to: outside\n    contact: topic",
			`share ask-papers: contact "topic" must name a master field of this process`},
		"a colleague message with a contact": {`to: colleague`, "to: colleague\n    contact: topic",
			"share heads_up: contact is only for an outside message"},
		"an unknown contact": {`to: outside`, "to: outside\n    contact: supplier", `contact "supplier" must name a master field`},
		"a long label":       {`label: Ask for the papers`, `label: ` + strings.Repeat("x", 41), "share ask-papers: label must be 1 to 40 letters"},
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

// TestListContact: a list says which of its text details are a phone and an email (D92), and a list
// rewritten with extra details keeps it.
func TestListContact(t *testing.T) {
	const list = "kind: supplier\nlabel: Supplier\nfields:\n  - { key: mobile, label: Mobile, type: text }\n" +
		"  - { key: mail, label: Mail, type: text }\n  - { key: since, label: Since, type: date }\n"
	m, err := ParseMasterKind([]byte(list + "contact: { phone: mobile, email: mail }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if *m.Contact != (Contact{Phone: "mobile", Email: "mail"}) {
		t.Errorf("contact = %+v", m.Contact)
	}
	more, err := m.WithDetails([]Field{{Key: "city", Label: "City", Type: "text"}})
	if err != nil || more.Contact == nil || *more.Contact != *m.Contact {
		t.Errorf("a list with an added detail lost its contact: %+v, %v", more, err)
	}

	// Everything else the list says survives too: its new_entries (so a new entry still starts as new)
	// and each detail's type, list, options and required.
	full, err := ParseMasterKind([]byte(`kind: supplier
label: Supplier
fields:
  - { key: mobile, label: Mobile, type: text, required: true }
  - { key: is_new, label: New, type: bool }
  - { key: note, label: Note, type: text }
  - { key: rating, label: Rating, type: number }
  - { key: size, label: Size, type: choice, options: [small, "big & bold"] }
  - { key: tags, label: Tags, type: list, of: text }
new_entries: { is_new: true, note: "Check <them> & \"call\"", rating: 2.5, size: "big & bold" }
contact: { phone: mobile }
`))
	if err != nil {
		t.Fatal(err)
	}
	again, err := full.WithDetails([]Field{{Key: "city", Label: "City", Type: "text"}})
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	if !reflect.DeepEqual(again.NewEntries, full.NewEntries) || *again.Contact != *full.Contact ||
		!reflect.DeepEqual(again.Fields[:len(full.Fields)], full.Fields) || again.Fields[len(full.Fields)].Key != "city" {
		t.Errorf("WithDetails lost something:\n got %+v\nwant %+v", again, full)
	}
	if only, err := ParseMasterKind([]byte(list + "contact: { phone: mobile }\n")); err != nil || only.Contact.Email != "" {
		t.Errorf("a phone alone: %v", err)
	}
	for contact, want := range map[string]string{
		"{ phone: mobile, email: web }": `contact: email: "web" must be a text detail of this list`,
		"{ phone: since }":              `contact: phone: "since" must be a text detail of this list`,
		"{}":                            "contact: name a phone or an email detail",
		"{ fax: mobile }":               "field fax not found",
	} {
		if _, err := ParseMasterKind([]byte(list + "contact: " + contact + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("contact %s: %v; want %q", contact, err, want)
		}
	}
}
