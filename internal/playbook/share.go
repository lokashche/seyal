// SPDX-License-Identifier: AGPL-3.0-or-later

package playbook

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// ShareMessage is a ready message a person may hand on from a work item (Phase 4, 4b, D91): to a
// colleague, or to someone outside the business. Seyal fills it in and shows it; the person checks it
// and hands it to their own app (email, the phone's share menu, the clipboard). Seyal never sends it.
type ShareMessage struct {
	ID    string   `yaml:"id"`    // e.g. ask-documents
	Label string   `yaml:"label"` // what the person picks, e.g. "Ask for the documents"; at most MaxShareLabel letters
	To    string   `yaml:"to"`    // ShareColleague or ShareOutside
	In    []string `yaml:"in"`    // the steps where it is offered; empty means every step
	Text  string   `yaml:"text"`  // with placeholders: field keys and the ShareBuiltins
}

// Who a share message is for.
const (
	ShareColleague = "colleague" // someone in the business, who can log in to Seyal
	ShareOutside   = "outside"   // someone outside the business, who has no login
)

// Share message limits.
const (
	MaxShareLabel = 40   // letters in a label
	MaxShareText  = 1500 // letters in a process's text
)

// BuiltinShare is the id of Seyal's own "Tell a colleague" message, offered on every process and every
// step. A process can't use the id for a message of its own.
const BuiltinShare = "colleague"

// ShareBuiltins are the placeholders every share text may use besides the process's field keys: the
// item's number, title, step and due date, its link in Seyal, the sharer's name and the business's
// name. A field with one of these keys would be ambiguous, so a process with share messages can't
// have one.
var ShareBuiltins = []string{"number", "title", "state", "due", "link", "me", "business"}

var shareIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Offered reports whether the message is offered in the step state.
func (m ShareMessage) Offered(state string) bool {
	return len(m.In) == 0 || slices.Contains(m.In, state)
}

// shareProblems checks the process's share messages. They are new in Phase 4, so it runs only for a
// version being added (see CheckNew). An outside message never carries the item's link: the person it
// goes to has no login, so it would only show them a login page and tell them where Seyal is.
func (p *Playbook) shareProblems() []string {
	if len(p.Share) == 0 {
		return nil
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	fields := map[string]bool{}
	for _, f := range p.Fields {
		fields[f.Key] = true
		if slices.Contains(ShareBuiltins, f.Key) {
			add("field %s: a process with share messages can't have a field called %q, because {%s} in a share text already means Seyal's own %s (rename the field)",
				f.Key, f.Key, f.Key, f.Key)
		}
	}
	seen := map[string]bool{}
	for i, m := range p.Share {
		where := fmt.Sprintf("share %d", i+1)
		if m.ID != "" {
			where = "share " + m.ID
		}
		switch {
		case !shareIDPattern.MatchString(m.ID):
			add("%s: id %q must start with a lowercase letter and use lowercase letters, digits, _ and -", where, m.ID)
		case m.ID == BuiltinShare:
			add("%s: %q is Seyal's own \"Tell a colleague\"; give the message another id", where, m.ID)
		case seen[m.ID]:
			add("%s: duplicate id", where)
		}
		seen[m.ID] = true
		if label := strings.TrimSpace(m.Label); label == "" || utf8.RuneCountInString(label) > MaxShareLabel {
			add("%s: label must be 1 to %d letters", where, MaxShareLabel)
		}
		if m.To != ShareColleague && m.To != ShareOutside {
			add("%s: to %q: use %s or %s", where, m.To, ShareColleague, ShareOutside)
		}
		for _, st := range m.In {
			if !slices.Contains(p.States, st) {
				add("%s: in: unknown state %q", where, st)
			}
		}
		if strings.TrimSpace(m.Text) == "" || utf8.RuneCountInString(m.Text) > MaxShareText {
			add("%s: text must be 1 to %d letters", where, MaxShareText)
		}
		for _, ph := range placeholder.FindAllStringSubmatch(m.Text, -1) {
			key := ph[1]
			switch {
			case key == "link" && m.To == ShareOutside:
				add("%s: an outside message can't carry {link}: the person it goes to can't log in to Seyal", where)
			case !slices.Contains(ShareBuiltins, key) && !fields[key]:
				add("%s: text: unknown placeholder {%s} (use a field key, or one of {%s})", where, key, strings.Join(ShareBuiltins, "}, {"))
			}
		}
	}
	return problems
}
