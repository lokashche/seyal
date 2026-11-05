// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Asking Seyal about the work (D67): "What is overdue?", "What did Ben finish this week?". The model
// only turns the question into a small search (a Query), checked here like any other input. Seyal
// runs the search itself (answer.go), on what the person may see, and writes the answer from a
// template. The model never sees the work items, so nothing in them reaches it, and text written
// inside an item can't steer it.

// Query is a question about the work, as the model reads it.
type Query struct {
	Kind      string   `json:"kind"`      // "query"; "cannot": about the work, but not a search Seyal can do; "other"
	Processes []string `json:"processes"` // process ids; none means every process
	Status    string   `json:"status"`    // one of statuses
	Person    string   `json:"person"`    // a name as said, "me" for the person asking, or ""
	Role      string   `json:"role"`      // what the person did, one of roles ("" without a person)
	Entry     string   `json:"entry"`     // the name of an entry in one of the business's lists, as said, or ""
	Period    string   `json:"period"`    // one of periods, or "" for any time
	Count     bool     `json:"count"`     // "how many": say the number only
}

var (
	queryKinds = []string{"query", "cannot", "other"}
	statuses   = []string{"open", "overdue", "finished", "cancelled", "any"}
	roles      = []string{"holds", "started", "finished", "approves"}
	periods    = []string{"today", "yesterday", "this_week", "last_week", "this_month", "last_month",
		"this_year", "last_year", "this_financial_year", "last_financial_year", "last_7_days", "last_30_days"}
)

// querySchema is the JSON schema of a Query, with the business's process ids as the only ones allowed.
func querySchema(known []string) map[string]any {
	process := map[string]any{"type": "string"}
	if len(known) > 0 {
		process["enum"] = known
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"kind", "processes", "status", "person", "role", "entry", "period", "count"},
		"properties": map[string]any{
			"kind":      map[string]any{"type": "string", "enum": queryKinds},
			"processes": map[string]any{"type": "array", "items": process},
			"status":    map[string]any{"type": "string", "enum": append([]string{""}, statuses...)},
			"person":    map[string]any{"type": "string"},
			"role":      map[string]any{"type": "string", "enum": append([]string{""}, roles...)},
			"entry":     map[string]any{"type": "string"},
			"period":    map[string]any{"type": "string", "enum": append([]string{""}, periods...)},
			"count":     map[string]any{"type": "boolean"},
		},
	}
}

// queryInstructions is the stable part of the request: the rules and the business's processes (their
// ids, names and descriptions; never items, people or list entries).
func queryInstructions(processes []Process) string {
	var b strings.Builder
	b.WriteString(`You turn a question that staff ask Seyal, a work assistant, into a small search over the business's
work items. Answer only with JSON that follows the given schema. You never see the work items: Seyal runs
the search and writes the answer itself.

kind:
- "query": the question asks which work items fit (or how many), using only the filters below.
- "cannot": the question is about the work, but these filters can't answer it (totals of amounts, reasons,
  opinions, details inside items, anything else).
- "other": not a question about the work items (how to use Seyal, small talk).

Filters (leave a filter empty when the question doesn't give it):
- processes: ids from the list below, only when the question names a kind of work; [] for all of them.
- status: "open" (pending, still to do, in progress, approved but still being done), "overdue" (open
  and past its due date), "finished" (done, completed), "cancelled", or "any". "" means open.
- person: the person's name exactly as said; "me" when the asker means themselves (I, me, my); "" for
  nobody in particular.
- role: what that person does with the work: "holds" (has it now, it is with them), "started" (created or
  asked for it), "finished" (completed or cancelled it), "approves" (it waits for their approval).
- entry: the name of what the work is for or about, as said, when it is a name (for example an
  organisation, a place or a project); "" otherwise. Never a person's name.
- period: when, as one of the given values, or "" for any time. For finished or cancelled work it is when
  the work was finished or cancelled; otherwise when it was started. "This financial year" and
  "this FY" are this_financial_year. If the question gives a time that none of the values fits (a date,
  "since March"), the kind is "cannot".
- count: true for "how many", false to list the items.

Processes:
`)
	for _, p := range processes {
		fmt.Fprintf(&b, "- id %s: %s. %s\n", p.ID, p.Name, p.Description)
	}
	return b.String()
}

// parseQuery reads the model's answer and checks it: every value must be one Seyal knows, and the
// filters must fit together. A process may be given by its id or its name, in any case ("Leave
// request" is leave-request). Missing parts get their plain meaning ("" status is open; a person
// without a role has the work, or finished it when the question is about finished work).
func parseQuery(text string, known []string) (Query, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	var q Query
	if err := json.Unmarshal([]byte(text), &q); err != nil {
		return Query{}, err
	}
	if !slices.Contains(queryKinds, q.Kind) {
		return Query{}, fmt.Errorf("unknown kind %q", q.Kind)
	}
	if q.Kind != "query" {
		return Query{Kind: q.Kind}, nil
	}
	var procs []string
	for _, said := range q.Processes {
		i := slices.IndexFunc(known, func(id string) bool { return sameProcess(id, said) })
		if i < 0 {
			return Query{}, fmt.Errorf("unknown process %q", said)
		}
		if !slices.Contains(procs, known[i]) {
			procs = append(procs, known[i])
		}
	}
	q.Processes = procs
	q.Person, q.Entry = tidyWords(q.Person), tidyWords(q.Entry)
	if slices.Contains([]string{"me", "i", "my", "myself", "mine"}, strings.ToLower(q.Person)) {
		q.Person = "me"
	}
	if q.Status == "" {
		q.Status = "open"
		if q.Role == "finished" {
			q.Status = "finished"
		}
	}
	if !slices.Contains(statuses, q.Status) {
		return Query{}, fmt.Errorf("unknown status %q", q.Status)
	}
	if q.Role != "" && !slices.Contains(roles, q.Role) {
		return Query{}, fmt.Errorf("unknown role %q", q.Role)
	}
	if q.Period != "" && !slices.Contains(periods, q.Period) {
		return Query{}, fmt.Errorf("unknown period %q", q.Period)
	}
	switch {
	case q.Role != "" && q.Person == "":
		q.Person = "me" // "what is waiting for approval" asks about the asker's own approvals
	case q.Role == "" && q.Person != "":
		q.Role = "holds"
		if q.Status == "finished" || q.Status == "cancelled" {
			q.Role = "finished"
		}
	}
	if q.Role == "finished" && q.Status != "finished" && q.Status != "cancelled" {
		q.Status = "finished" // only finished or cancelled work has someone who finished it
	}
	return q, nil
}

// sameProcess reports whether said names the process id: the id itself or its name, ignoring case,
// spaces, "-" and "_".
func sameProcess(id, said string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(s))), " ")
	}
	return norm(said) != "" && norm(said) == norm(id)
}

// tidyWords trims a name, joins its spaces, and keeps it short.
func tidyWords(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 100 {
		s = string([]rune(s)[:100])
	}
	return s
}

// span is a stretch of time in the business's time zone: from (included) to (not included), and
// how to say it.
type span struct {
	From, To time.Time
	Words    string
}

// periodSpan works out a period from now, which must be in the business's time zone. Weeks start on
// Monday; the financial year starts on the first day of fiscalStart (1-12).
func periodSpan(period string, now time.Time, fiscalStart int) (span, bool) {
	loc := now.Location()
	y, m, d := now.Date()
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, loc) }
	days := func(from, to time.Time) string { // "2 Nov – 8 Nov"
		last := to.AddDate(0, 0, -1)
		if from.Equal(last) {
			return from.Format("2 Jan")
		}
		return from.Format("2 Jan") + " – " + last.Format("2 Jan")
	}
	monday := day(y, m, d-(int(now.Weekday())+6)%7)
	fy := workitem.FiscalYear(now, fiscalStart)
	var s span
	switch period {
	case "today":
		s = span{From: day(y, m, d), To: day(y, m, d+1)}
		s.Words = "today (" + days(s.From, s.To) + ")"
	case "yesterday":
		s = span{From: day(y, m, d-1), To: day(y, m, d)}
		s.Words = "yesterday (" + days(s.From, s.To) + ")"
	case "this_week", "last_week":
		s, s.Words = span{From: monday, To: monday.AddDate(0, 0, 7)}, "this week"
		if period == "last_week" {
			s, s.Words = span{From: monday.AddDate(0, 0, -7), To: monday}, "last week"
		}
		s.Words += " (" + days(s.From, s.To) + ")"
	case "this_month", "last_month":
		s, s.Words = span{From: day(y, m, 1), To: day(y, m+1, 1)}, "this month"
		if period == "last_month" {
			s, s.Words = span{From: day(y, m-1, 1), To: day(y, m, 1)}, "last month"
		}
		s.Words += " (" + s.From.Format("January 2006") + ")"
	case "this_year", "last_year":
		s, s.Words = span{From: day(y, 1, 1), To: day(y+1, 1, 1)}, "this year"
		if period == "last_year" {
			s, s.Words = span{From: day(y-1, 1, 1), To: day(y, 1, 1)}, "last year"
		}
		s.Words += " (" + s.From.Format("2006") + ")"
	case "this_financial_year", "last_financial_year":
		start := time.Month(fiscalStart)
		s, s.Words = span{From: day(fy, start, 1), To: day(fy+1, start, 1)}, "this financial year"
		if period == "last_financial_year" {
			s, s.Words = span{From: day(fy-1, start, 1), To: day(fy, start, 1)}, "last financial year"
		}
		s.Words += " (" + s.From.Format("Jan 2006") + " – " + s.To.AddDate(0, 0, -1).Format("Jan 2006") + ")"
	case "last_7_days", "last_30_days":
		n := 7
		if period == "last_30_days" {
			n = 30
		}
		s = span{From: day(y, m, d-n+1), To: day(y, m, d+1)}
		s.Words = fmt.Sprintf("in the last %d days (%s)", n, days(s.From, s.To))
	default:
		return span{}, false
	}
	return s, true
}

// contains reports whether t falls in the span.
func (s span) contains(t time.Time) bool { return !t.Before(s.From) && t.Before(s.To) }

// stage says where a state of a process stands, for questions: "open" (the work goes on),
// "finished" or "cancelled". Open is exactly what My Work shows as open: any state the work can still
// leave, even if only by cancelling (such as "approved", while its holder does the work). The rule is
// engine.IsClosed, the one My Work uses too (through workitem.List), so it is never copied here. Of the
// closed states, those reached only by an action that needs a reason (such as cancel) are
// "cancelled"; the others are "finished".
func stage(p *playbook.Playbook, state string) string {
	if !engine.IsClosed(p, state) {
		return "open"
	}
	byReason, otherwise := false, p.Approvals != nil && p.Approvals.OnComplete == state
	for _, t := range p.Transitions {
		if t.To == state {
			if t.Needs == "reason" {
				byReason = true
			} else {
				otherwise = true
			}
		}
	}
	if byReason && !otherwise {
		return "cancelled"
	}
	return "finished"
}

// looksLikeQuestion reports whether a message is clearly a question about something, without AI:
// it starts with a question word and ends with a question mark. "Can you fix the printer?" is a
// request, so it doesn't count.
func looksLikeQuestion(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if !strings.HasSuffix(t, "?") {
		return false
	}
	first, _, _ := strings.Cut(t, " ")
	first = strings.TrimRightFunc(first, func(r rune) bool { return !unicode.IsLetter(r) })
	return slices.Contains([]string{"what", "what's", "whats", "which", "who", "who's", "whose"}, first) ||
		strings.HasPrefix(t, "how many ")
}

// processName says a process id in words: "leave-request" → "Leave request".
func processName(id string) string {
	s := strings.NewReplacer("_", " ", "-", " ").Replace(id)
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// describe writes the answer's line: how many items, of what, and the filters, e.g. "2 open items
// for Ganga Traders" or "1 item finished by Priya Raman this week (2 Nov – 8 Nov)". who is the person
// as the line names them ("you" for the asker).
func describe(q Query, n int, processes []string, who, entry string, when *span) string {
	var b strings.Builder
	if n == 0 {
		b.WriteString("No ")
	} else {
		fmt.Fprintf(&b, "%d ", n)
	}
	ended := q.Status == "finished" || q.Status == "cancelled"
	if q.Status != "any" && !(ended && (q.Role == "finished" || when != nil)) { // not "finished items finished …"
		b.WriteString(q.Status + " ")
	}
	if n == 1 {
		b.WriteString("item")
	} else {
		b.WriteString("items")
	}
	if len(processes) > 0 {
		b.WriteString(" of " + strings.Join(processes, " or "))
	}
	if entry != "" {
		b.WriteString(" for " + entry)
	}
	switch q.Role {
	case "holds":
		b.WriteString(" with " + who)
	case "started":
		b.WriteString(" started by " + who)
	case "finished":
		b.WriteString(" " + q.Status + " by " + who)
	case "approves":
		if who == "you" {
			b.WriteString(" waiting for your approval")
		} else {
			b.WriteString(" waiting for " + who + "'s approval")
		}
	}
	if when != nil {
		switch {
		case q.Role == "started" || q.Role == "finished":
		case ended:
			b.WriteString(" " + q.Status)
		default:
			b.WriteString(" started")
		}
		b.WriteString(" " + when.Words)
	}
	return b.String()
}
