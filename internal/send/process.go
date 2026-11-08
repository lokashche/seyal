// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"seyal/internal/playbook"
)

// Sends lists a process's `do: send` actions, by the state that runs them, in a fixed order.
func Sends(p *playbook.Playbook) []StateSend {
	var out []StateSend
	for _, state := range slices.Sorted(maps.Keys(p.On)) {
		for _, a := range p.On[state] {
			if a.Do == "send" {
				out = append(out, StateSend{State: state, Action: a})
			}
		}
	}
	return out
}

// StateSend is one `do: send` of a process and the state whose entry runs it.
type StateSend struct {
	State  string
	Action playbook.Action
}

// CheckProcess reports, for a process being switched on in a business, every `do: send` the business's
// other apps (targets) don't allow (D96, design 2.3 "In the process file"): the app must be listed,
// every field must be on the app's own list (what the maintainer agreed it may ever receive), and the
// link may go only to an app that allows it. The messages never hold an address.
func CheckProcess(targets []Target, p *playbook.Playbook) []string {
	byName := map[string]Target{}
	for _, t := range targets {
		byName[t.Name] = t
	}
	var problems []string
	for _, s := range Sends(p) {
		where := fmt.Sprintf("on %s: send to %s", s.State, s.Action.To)
		t, ok := byName[s.Action.To]
		if !ok {
			problems = append(problems, where+": the business's set-up file lists no other app called "+s.Action.To)
			continue
		}
		for _, f := range s.Action.Fields {
			if !slices.Contains(t.Fields, f) {
				problems = append(problems, fmt.Sprintf("%s: %s may not receive %s (its fields in the set-up file are: %s)",
					where, t.Name, f, list(t.Fields)))
			}
		}
		if s.Action.Link && !t.Link {
			problems = append(problems, where+": "+t.Name+" may not receive the item's link (link: true in the set-up file allows it)")
		}
	}
	return problems
}

// list writes keys as "a, b", or "none".
func list(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}
