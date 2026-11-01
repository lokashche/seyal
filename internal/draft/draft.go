// SPDX-License-Identifier: AGPL-3.0-or-later

// Package draft asks AI to write a process definition from plain words: a change to a process the
// business has, or a new one. The model only drafts. Every draft is checked exactly like a definition
// typed by hand, and nothing is saved until a person has looked at it and pressed Save. If a draft
// has problems, the model is shown them and gets one more try.
package draft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/playbook"
)

// Result is a draft, checked.
type Result struct {
	Summary  string                 // what the model says it changed
	Source   string                 // the drafted process definition
	Playbook *playbook.Playbook     // parsed, when it has no problems
	Lists    []*playbook.MasterKind // new or changed lists the draft needs
	Problems []string               // what is still wrong; empty when it can be saved
}

// OK reports whether the draft can be saved as it is.
func (r Result) OK() bool { return len(r.Problems) == 0 && r.Playbook != nil }

// ErrOff means AI drafting is not switched on.
var ErrOff = errors.New("AI drafting is not switched on")

// Process drafts a process from words. current is the process to change, or nil for a new one;
// templates give a sample definition when there is no current one. tx must come from db.WithTenant.
func Process(ctx context.Context, tx pgx.Tx, setup *ai.Setup, templates *playbook.Library,
	current *playbook.Playbook, words, personID string) (Result, error) {
	if _, on := setup.For(ai.Draft); !on {
		return Result{}, ErrOff
	}
	words = strings.TrimSpace(words)
	if words == "" {
		return Result{}, errors.New("please describe the change")
	}
	instr, err := instructions(ctx, tx, templates, current)
	if err != nil {
		return Result{}, err
	}
	input := request(current, words)

	var res Result
	for try := 0; try < 2; try++ {
		resp, err := setup.Call(ctx, tx, ai.Draft,
			ai.Request{Instructions: instr, Input: input, Schema: schema, MaxTokens: 8000},
			ai.Link{PersonID: personID})
		if err != nil {
			return Result{}, err
		}
		res = fromAnswer(ctx, tx, current, resp.Text)
		if res.OK() {
			return res, nil
		}
		input = request(current, words) + "\nYour last draft had these problems. Fix them and answer again:\n- " +
			strings.Join(res.Problems, "\n- ") + "\n\nYour last draft:\n" + res.Source
	}
	return res, nil
}

// answer is what the model returns.
type answer struct {
	Summary    string   `json:"summary"`
	Definition string   `json:"definition"`
	Lists      []string `json:"lists"`
}

var schema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"summary", "definition", "lists"},
	"properties": map[string]any{
		"summary":    map[string]any{"type": "string"},
		"definition": map[string]any{"type": "string"},
		"lists":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

// fromAnswer reads the model's answer and checks the draft.
func fromAnswer(ctx context.Context, tx pgx.Tx, current *playbook.Playbook, text string) Result {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	var a answer
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		return Result{Problems: []string{"the answer was not the JSON asked for: " + err.Error()}}
	}
	res := Check(ctx, tx, current, a.Definition, a.Lists)
	res.Summary = strings.TrimSpace(a.Summary)
	return res
}

// Check checks a draft (a definition, and the lists it adds or changes) against the business's lists
// and roles, as if it were saved. Lists identical to the business's own are left out of the result.
// current is the process it changes, or nil for a new one. tx must come from db.WithTenant.
func Check(ctx context.Context, tx pgx.Tx, current *playbook.Playbook, definition string, lists []string) Result {
	res := Result{Source: strings.TrimSpace(strings.ReplaceAll(definition, "\r\n", "\n")) + "\n"}
	add := func(format string, args ...any) { res.Problems = append(res.Problems, fmt.Sprintf(format, args...)) }

	lib, err := playbook.ForBusiness(ctx, tx, nil)
	if err != nil {
		add("%v", err)
		return res
	}
	for _, src := range lists {
		if strings.TrimSpace(src) == "" {
			continue
		}
		mk, err := playbook.ParseMasterKind([]byte(strings.TrimSpace(strings.ReplaceAll(src, "\r\n", "\n")) + "\n"))
		if err != nil {
			add("list: %v", err)
			continue
		}
		if old := lib.Masters[mk.Kind]; old != nil && old.Hash == mk.Hash {
			continue // unchanged
		}
		lib.Masters[mk.Kind] = mk
		res.Lists = append(res.Lists, mk)
	}

	p, err := playbook.Parse([]byte(res.Source))
	if err != nil {
		add("definition: %v", err)
		return res
	}
	if err := lib.Check(p); err != nil {
		add("%v", err)
	}
	if current != nil && p.ID != current.ID {
		add("id must stay %q", current.ID)
	}
	if current != nil && p.Version == current.Version {
		add("version must change (it is %q now)", current.Version)
	}
	if current == nil {
		inUse, err := playbook.ListInUse(ctx, tx)
		if err != nil {
			add("%v", err)
		}
		if slices.ContainsFunc(inUse, func(u playbook.InUse) bool { return u.PlaybookID == p.ID }) {
			add("there is already a process with the id %q; choose another id, or change that process instead", p.ID)
		}
	}
	roles, err := businessRoles(ctx, tx)
	if err != nil {
		add("%v", err)
	}
	for _, r := range p.Roles {
		if !slices.Contains(roles, r) {
			add("role %q doesn't exist in this business (use one of: %s)", r, strings.Join(roles, ", "))
		}
	}
	if len(res.Problems) == 0 {
		res.Playbook = p
	}
	return res
}

func businessRoles(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, _ := tx.Query(ctx, `SELECT key FROM roles WHERE tenant_id = current_tenant() ORDER BY key`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// instructions is the stable part: the format, the business's roles and lists, and an example.
func instructions(ctx context.Context, tx pgx.Tx, templates *playbook.Library, current *playbook.Playbook) (string, error) {
	var b strings.Builder
	b.WriteString(guide)
	roles, err := businessRoles(ctx, tx)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\nRoles in this business: %s\n", strings.Join(roles, ", "))

	kinds, err := playbook.Kinds(ctx, tx)
	if err != nil {
		return "", err
	}
	b.WriteString("\nLists this business has (master kinds; a field of type master chooses from one):\n")
	for _, k := range slices.Sorted(maps.Keys(kinds)) {
		b.WriteString("---\n" + string(kinds[k].Source))
	}
	if current == nil && templates != nil {
		if ids := slices.Sorted(maps.Keys(templates.Playbooks)); len(ids) > 0 {
			b.WriteString("\nAn example process definition (a sample, for the format only):\n---\n")
			b.WriteString(string(templates.Playbooks[ids[0]].Source))
		}
	}
	return b.String(), nil
}

// request is the changing part: the current definition and the words.
func request(current *playbook.Playbook, words string) string {
	if current == nil {
		return "Write a new process definition for this description:\n" + words + "\n"
	}
	return "The current definition:\n---\n" + string(current.Source) + "---\n\nChange it as asked:\n" + words + "\n"
}

// guide explains the definition format. It is generic: the words of a business belong in its
// definitions, not here.
const guide = `You write process definitions for Seyal, a work assistant. A process says what details a piece of
work needs, the steps it goes through, who approves it and what Seyal does by itself. Definitions are
YAML. Answer only with JSON that follows the given schema:
- "summary": two or three plain sentences saying what you changed, for the person who asked.
- "definition": the complete process definition as YAML text.
- "lists": the complete YAML text of each list you add or change (leave unchanged lists out).
Change only what was asked. Keep every key, label and wording that wasn't mentioned.

Process definition keys:
- id: lowercase-with-dashes; it never changes for an existing process.
- version: free text; it must change on every change (e.g. "0.1" becomes "0.2").
- description: one sentence. examples: a few sample messages people would send to start one.
- numbering: how items are numbered, with {seq:N} exactly once and optionally {yyyy} or {yy}, e.g. "R-{yyyy}-{seq:4}".
- title: a title made from field keys in braces, e.g. "{place} - {kind}".
- roles: the roles the approvals use (only roles of this business).
- fields: each { key, label, type, required, readonly, ask, ... }. key is lowercase_with_underscores.
  types: text, number, money, date, choice (needs options: [a, b]; use [yes, no] for a yes/no question), person,
  list (needs of: text, number, date or person),
  master (needs kind: a list's kind; may have prefill: { this_field: list_field } to copy details from the chosen entry).
  ask is the question Seyal asks in the chat when the field is still empty; give one for every required field.
- states: the steps, lowercase_with_underscores. initial: the first state.
- transitions: each { action, from: [states], to, when: required_filled (optional), needs: reason (optional) }.
- approvals (optional): { in: state, on_complete: state, on_reject: state, slots: [ { slot, role, required_if } ] }.
  A slot without required_if is always needed. required_if is a condition:
  { field: key, eq|ne|lt|gt: value } where value is a plain value or { field: other_key };
  { any: [conditions] } or { all: [conditions] }. lt and gt work on number, money and date fields.
  key.detail refers to a detail of the chosen list entry, e.g. { field: kind.is_new, eq: true }.
- deadlines (optional): [ { due_from: a date field } ].
- on (optional): actions when entering a state: { state: [ { do: post, text: "..." }, { do: assign, to: person_field } ] }.
  post text may use {field_key}.

List definition: { kind: lowercase_with_underscores, label: Singular label, fields: [ { key, label, type, ... } ] }.
List fields use the types text, number, money, date, bool, choice, person, list; no ask, kind or prefill.
Every list entry also has a built-in key and name; don't add those as fields.
`
