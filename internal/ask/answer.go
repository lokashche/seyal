// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Answer is Seyal's answer to a question about the work: one line, then the items it found (at most
// shown of them) and how many more there are. It is shown once and not kept: asking changes nothing,
// so it isn't a ledger event (D67).
type Answer struct {
	Question string
	Line     string
	Items    []Found
	More     int
	// MessageID is the saved message when Seyal couldn't read it (AI off, or failing just now). It may
	// have been new work worded as a question, so the screen offers to start it as work instead.
	MessageID string
}

// Asked is a question about the work that Begin or Route saved but left to answer. The caller answers
// it with AnswerAsked in a new transaction, after the one that saved the message: the search and its
// AI call then hold no lock that other changes in the business wait for (D67).
type Asked struct {
	MessageID, PersonID, Question string
	Reply                         string // Seyal's reply if the model reads it as no question about the work items
}

// Found is one item in an answer.
type Found struct {
	ID, Number, Title, Process, Status string
	With                               string // who has it now, or ""
}

// shown is how many items an answer lists.
const shown = 8

// Lines Seyal answers with when it can't search.
const (
	needsAI = "Answering questions about the work needs AI, and AI is off for this business. My Work shows " +
		"what is open and overdue. If you meant new work, start it with a button below."
	cannotSearch = "I can't look that up yet. I can list or count work by process, by status (open, overdue, " +
		"finished, cancelled), by person (who has it, who started or finished it, who must approve it), by a " +
		"name from your lists, and by time (today, this week, last month, this financial year). For example: " +
		"“What is overdue?”, “What did I finish this week?”, “How many items are waiting for my approval?”"
	notNow = "Sorry, I couldn't work that question out just now. Please try again, or look in My Work. " +
		"If you meant new work, start it with a button below."
	// Busy is the line for a person who asked many questions in a short time.
	Busy = "You've asked a lot of questions in the last minute. Please wait a moment and ask again. " +
		"If you meant new work, start it with a button below."
)

// queryTimeout limits the call that turns a question into a search: its answer is short, and the
// person is waiting.
const queryTimeout = 30 * time.Second

// searchRoom is the time the search needs after that call: recording the call, then running the
// search on the items.
const searchRoom = 5 * time.Second

// Unread answers a message that is clearly a question when Seyal can't read it now: with line "",
// because AI is off or failed just now. Nothing starts; the answer offers to start the message as
// work instead.
func Unread(env Env, messageID, question, line string) *Answer {
	if line == "" {
		line = notNow
		if _, on := env.AI.For(ai.Understand); !on {
			line = needsAI
		}
	}
	return &Answer{Question: strings.TrimSpace(question), Line: line, MessageID: messageID}
}

// AnswerAsked answers a question about the work that Begin or Route left to answer. With AI on, the
// model turns it into a Query; Seyal checks the query and runs it on the items the person may see
// (workitem.Visible). When the model reads the message as no question about the work items, Seyal
// posts its reply instead (unfiled), and the answer is nil. When ctx has a deadline too near for the
// search call (the first call was slow), Seyal doesn't make it and answers as when AI fails just now.
// tx must come from db.WithTenant, and should be a new transaction, not the one that saved the
// message (see Asked).
func AnswerAsked(ctx context.Context, tx pgx.Tx, env Env, q Asked) (*Answer, []channel.Message, error) {
	a, err := answerAt(ctx, tx, env, q, time.Now())
	if err != nil || a != nil {
		return a, nil, err
	}
	m, err := channel.Post(ctx, tx, "", limit(cmp.Or(strings.TrimSpace(q.Reply), greetingReply), 600))
	if err != nil {
		return nil, nil, err
	}
	return nil, []channel.Message{m}, nil
}

// answerAt answers q at now. It returns nil when the message isn't a question about the work items.
func answerAt(ctx context.Context, tx pgx.Tx, env Env, q Asked, now time.Time) (*Answer, error) {
	question := strings.TrimSpace(q.Question)
	personID := q.PersonID
	say := func(line string) (*Answer, error) { return &Answer{Question: question, Line: line}, nil }
	// The search failed or can't be used. A clear question still gets an answer, rather than the
	// first reply (which may promise a look-up that never comes); anything else gets that reply.
	unsure := func() (*Answer, error) {
		if looksLikeQuestion(question) {
			return Unread(env, q.MessageID, question, notNow), nil
		}
		return nil, nil
	}
	if _, on := env.AI.For(ai.Understand); !on {
		return Unread(env, q.MessageID, question, needsAI), nil
	}
	// The person's page must come back before the web server's time limit; a search call cut off
	// by it would still cost money and leave them with a dropped connection.
	if end, ok := ctx.Deadline(); ok && time.Until(end) < queryTimeout+searchRoom {
		return unsure()
	}
	processes, err := activeProcesses(ctx, tx)
	if err != nil {
		return nil, err
	}
	known := make([]string, len(processes))
	for i, p := range processes {
		known[i] = p.ID
	}
	resp, err := env.AI.Call(ctx, tx, ai.Understand, ai.Request{Instructions: queryInstructions(processes),
		Input: "New message from the person:\n" + question + "\n", Schema: querySchema(known), MaxTokens: 2048,
		Timeout: queryTimeout}, ai.Link{PersonID: personID})
	if err != nil {
		return unsure() // AI is down or slow
	}
	query, err := parseQuery(resp.Text, known)
	switch {
	case err != nil:
		return unsure()
	case query.Kind == "other":
		return nil, nil
	case query.Kind == "cannot":
		return say(cannotSearch)
	}
	a, err := run(ctx, tx, personID, query, now)
	if a != nil {
		a.Question = question
	}
	return a, err
}

// activeProcesses lists the current version of each process the business has switched on.
func activeProcesses(ctx context.Context, tx pgx.Tx) ([]Process, error) {
	rows, _ := tx.Query(ctx, `
		SELECT pv.id::text, pv.source FROM tenant_playbooks tp
		JOIN playbook_versions pv ON pv.tenant_id = tp.tenant_id AND pv.id = tp.current_version_id
		WHERE tp.tenant_id = current_tenant() AND tp.active ORDER BY tp.playbook_id`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Source string }])
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, pv := range list {
		p, err := playbook.Parse([]byte(pv.Source))
		if err != nil {
			return nil, err
		}
		out = append(out, Process{VersionID: pv.ID, ID: p.ID, Name: processName(p.ID), Description: p.Description})
	}
	return out, nil
}

// run carries out a checked query for personID, at now, and writes the answer.
func run(ctx context.Context, tx pgx.Tx, personID string, q Query, now time.Time) (*Answer, error) {
	say := func(line string) (*Answer, error) { return &Answer{Line: line}, nil }
	var zone string
	var fiscalStart int
	if err := tx.QueryRow(ctx, `SELECT timezone, fiscal_year_start_month FROM tenants WHERE id = current_tenant()`).
		Scan(&zone, &fiscalStart); err != nil {
		return nil, fmt.Errorf("ask: business settings: %w", err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	now = now.In(loc)
	today := now.Format(time.DateOnly)
	var when *span
	if q.Period != "" {
		s, _ := periodSpan(q.Period, now, fiscalStart)
		when = &s
	}

	people, err := pairList(ctx, tx, `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant()`)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range people {
		names[p.Key] = p.Name
	}

	// Who the question is about: one person, or every person with exactly that name (Seyal can't tell
	// them apart by name, so the answer covers them all and says so).
	var persons []string
	who := ""
	personRoles := map[string][]string{}
	if q.Person != "" {
		if q.Person == "me" {
			persons, who = []string{personID}, "you"
		} else {
			ids := matchName(q.Person, people)
			found := distinctNames(ids, people)
			switch {
			case len(ids) == 0:
				return say(fmt.Sprintf("I couldn't find anyone called “%s” in this business.", q.Person))
			case len(found) > 1:
				return say(fmt.Sprintf("More than one person is called “%s”: %s. Please say the full name.",
					q.Person, strings.Join(found, ", ")))
			}
			persons, who = ids, found[0]
			if len(ids) > 1 {
				who = fmt.Sprintf("%s (all %d people with that name)", found[0], len(ids))
			}
		}
		rows, _ := tx.Query(ctx, `
			SELECT person_id::text, role_key FROM person_roles
			WHERE tenant_id = current_tenant() AND person_id::text = ANY($1) AND revoked_at IS NULL`, persons)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Person, Role string }])
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			personRoles[r.Person] = append(personRoles[r.Person], r.Role)
		}
	}

	// What the work is for: entries of the business's lists with that name ("kind/key").
	var entries []string
	entry := ""
	if q.Entry != "" {
		list, err := pairList(ctx, tx, `SELECT kind || '/' || key, name FROM master_items WHERE tenant_id = current_tenant()`)
		if err != nil {
			return nil, err
		}
		entries = matchName(q.Entry, list)
		found := distinctNames(entries, list)
		switch {
		case len(found) == 0:
			return say(fmt.Sprintf("I couldn't find “%s” in any of the business's lists.", q.Entry))
		case len(found) > 1:
			if len(found) > 5 {
				found = append(found[:5], "…")
			}
			return say(fmt.Sprintf("Which one do you mean: %s?", strings.Join(found, ", ")))
		}
		entry = found[0]
	}

	var processNames []string
	for _, id := range q.Processes {
		processNames = append(processNames, processName(id))
	}

	all, err := workitem.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	if all, err = workitem.Visible(ctx, tx, personID, all); err != nil {
		return nil, err
	}
	playbooks := map[string]*playbook.Playbook{}
	type hit struct {
		it    workitem.Item
		p     *playbook.Playbook
		stage string
	}
	var hits []hit
	for _, it := range all {
		p := playbooks[it.PlaybookVersionID]
		if p == nil {
			if p, err = workitem.PlaybookFor(ctx, tx, it); err != nil {
				return nil, err
			}
			playbooks[it.PlaybookVersionID] = p
		}
		st := stage(p, it.State)
		late := st == "open" && it.DueOn != "" && it.DueOn < today
		switch {
		case len(q.Processes) > 0 && !slices.Contains(q.Processes, p.ID),
			q.Status == "overdue" && !late,
			q.Status != "overdue" && q.Status != "any" && q.Status != st,
			len(entries) > 0 && !holdsEntry(p, it, entries):
			continue
		}
		switch q.Role {
		case "holds":
			if !slices.ContainsFunc(persons, func(person string) bool {
				return it.AssigneeID == person || it.AssigneeID == "" && it.Team != "" && slices.Contains(personRoles[person], it.Team)
			}) {
				continue
			}
		case "started":
			if !slices.Contains(persons, it.CreatedBy) {
				continue
			}
		case "approves":
			if p.Approvals == nil || it.State != p.Approvals.In {
				continue
			}
			ap, err := workitem.Approvals(ctx, tx, it.ID)
			if err != nil {
				return nil, err
			}
			if !slices.ContainsFunc(persons, func(person string) bool { return mayDecide(ap, person, personRoles[person]) }) {
				continue
			}
		}
		hits = append(hits, hit{it, p, st})
	}

	// When each item started, and when (and by whom) the ended ones ended.
	ended := q.Status == "finished" || q.Status == "cancelled"
	at := map[string]time.Time{}
	by := map[string]string{}
	if ended {
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.it.ID
		}
		if at, by, err = endings(ctx, tx, ids); err != nil {
			return nil, err
		}
	} else if when != nil {
		rows, _ := tx.Query(ctx, `SELECT id::text, created_at FROM work_items WHERE tenant_id = current_tenant()`)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			ID string
			At time.Time
		}])
		if err != nil {
			return nil, err
		}
		for _, l := range list {
			at[l.ID] = l.At
		}
	}
	hits = slices.DeleteFunc(hits, func(h hit) bool {
		return (when != nil && !when.contains(at[h.it.ID])) || (q.Role == "finished" && !slices.Contains(persons, by[h.it.ID]))
	})
	switch {
	case q.Status == "overdue":
		slices.SortStableFunc(hits, func(a, b hit) int { return strings.Compare(a.it.DueOn, b.it.DueOn) })
	case ended:
		slices.SortStableFunc(hits, func(a, b hit) int { return at[b.it.ID].Compare(at[a.it.ID]) })
	}

	a := &Answer{Line: describe(q, len(hits), processNames, who, entry, when)}
	if q.Count || len(hits) == 0 {
		a.Line += "."
		return a, nil
	}
	a.Line += ":"
	roleNames, err := pairList(ctx, tx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	if err != nil {
		return nil, err
	}
	for i, h := range hits {
		if i == shown {
			a.More = len(hits) - shown
			break
		}
		f := Found{ID: h.it.ID, Number: h.it.Number, Title: h.it.Title, Process: processName(h.p.ID),
			Status: strings.NewReplacer("_", " ", "-", " ").Replace(h.it.State)}
		if h.stage == "open" && h.it.DueOn != "" && h.it.DueOn < today {
			f.Status += " · overdue"
		}
		switch {
		case h.stage != "open":
		case h.it.AssigneeID != "":
			f.With = names[h.it.AssigneeID]
		case h.it.Team != "":
			f.With = cmp.Or(lookup(roleNames, h.it.Team), h.it.Team) + " team"
		}
		if !strings.ContainsFunc(f.Title, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			f.Title = "Untitled"
			var first string
			err := tx.QueryRow(ctx, `SELECT text FROM messages WHERE tenant_id = current_tenant() AND id::text = $1`,
				h.it.StartedRef).Scan(&first)
			if first = strings.Join(strings.Fields(first), " "); err == nil && first != "" {
				f.Title = "“" + limit(first, 70) + "”"
			}
		}
		a.Items = append(a.Items, f)
	}
	return a, nil
}

// named is a key and the name people know it by: a person, a role or a list entry.
type named struct{ Key, Name string }

func pairList(ctx context.Context, tx pgx.Tx, sql string) ([]named, error) {
	rows, _ := tx.Query(ctx, sql)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[named])
}

func lookup(list []named, key string) string {
	if i := slices.IndexFunc(list, func(n named) bool { return n.Key == key }); i >= 0 {
		return list[i].Name
	}
	return ""
}

// matchName finds the keys whose name fits what was said, ignoring case and spaces: the same name if
// there is one; otherwise names that have every word said ("Ben" fits "Ben Example"); otherwise
// names that contain what was said.
func matchName(said string, list []named) []string {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	s := norm(said)
	var exact, words, part []string
	for _, n := range list {
		name := norm(n.Name)
		switch {
		case name == s:
			exact = append(exact, n.Key)
		case hasWords(name, s):
			words = append(words, n.Key)
		case s != "" && strings.Contains(name, s):
			part = append(part, n.Key)
		}
	}
	switch {
	case len(exact) > 0:
		return exact
	case len(words) > 0:
		return words
	}
	return part
}

// hasWords reports whether every word of said is one of the name's words.
func hasWords(name, said string) bool {
	have := strings.Fields(name)
	want := strings.Fields(said)
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return len(want) > 0
}

// distinctNames lists the different names of the given keys, in the list's order.
func distinctNames(keys []string, list []named) []string {
	var out []string
	for _, n := range list {
		if slices.Contains(keys, n.Key) && !slices.Contains(out, n.Name) {
			out = append(out, n.Name)
		}
	}
	return out
}

// holdsEntry reports whether one of the item's list fields holds one of the entries ("kind/key").
func holdsEntry(p *playbook.Playbook, it workitem.Item, entries []string) bool {
	for _, f := range p.Fields {
		if f.Kind == "" || !(f.Type == "master" || f.Type == "list" && f.Of == "master") {
			continue
		}
		var keys []string
		switch v := it.Fields[f.Key].(type) {
		case string:
			keys = []string{v}
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					keys = append(keys, s)
				}
			}
		}
		for _, k := range keys {
			if slices.Contains(entries, f.Kind+"/"+k) {
				return true
			}
		}
	}
	return false
}

// mayDecide reports whether the person may decide one of the approvals the item waits for now: one
// of their roles, and they haven't decided another approval of this version (as on the screens).
func mayDecide(st engine.ApprovalStatus, personID string, roles []string) bool {
	for _, slot := range st.Slots {
		if slot.ApprovedBy == personID {
			return false
		}
	}
	for _, slot := range st.Slots {
		if slices.Contains(st.Waiting, slot.Slot) && slices.Contains(roles, slot.Role) {
			return true
		}
	}
	return false
}

// endings reads from the ledger when each of the items reached the state it is in now, and who
// moved it there. When Seyal moved it on its own (for example after the last approval), the person
// who acted on it last before that counts.
func endings(ctx context.Context, tx pgx.Tx, ids []string) (map[string]time.Time, map[string]string, error) {
	at, by := map[string]time.Time{}, map[string]string{}
	if len(ids) == 0 {
		return at, by, nil
	}
	rows, _ := tx.Query(ctx, `
		SELECT subject_id::text, kind, occurred_at,
			CASE actor_kind WHEN 'human' THEN coalesce(actor_person_id::text, '')
				WHEN 'agent' THEN coalesce(on_behalf_of::text, '') ELSE '' END
		FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id::text = ANY($1)
		ORDER BY seq`, ids)
	events, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Item, Kind string
		At         time.Time
		Person     string
	}])
	if err != nil {
		return nil, nil, fmt.Errorf("ask: endings: %w", err)
	}
	last := map[string]string{} // the last person who acted on each item so far
	for _, e := range events {
		if e.Kind == "item.state_changed" {
			at[e.Item], by[e.Item] = e.At, cmp.Or(e.Person, last[e.Item])
		}
		if e.Person != "" {
			last[e.Item] = e.Person
		}
	}
	return at, by, nil
}
