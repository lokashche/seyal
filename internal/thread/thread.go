// SPDX-License-Identifier: AGPL-3.0-or-later

// Package thread builds a work item's timeline: its messages and voice notes, and everything that
// happened to it, in time order. Events are described in plain words using the item's own
// playbook (field labels, state names), so the engine itself stays free of business words.
package thread

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Entry is one line of a thread: a message, or something that happened.
type Entry struct {
	At      time.Time
	Kind    string           // "message", or the ledger event kind, e.g. "item.decided"
	Who     string           // e.g. "Sam Sales", "Seyal", "Seyal for Sam Sales"
	Text    string           // what happened or what was said, in plain words
	Message *channel.Message // the message itself, with its files (messages only)
	Seq     int64            // ledger position (events only)
}

// For returns the thread of one work item, oldest first. tx must come from db.WithTenant.
func For(ctx context.Context, tx pgx.Tx, itemID string) ([]Entry, error) {
	it, err := workitem.Get(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	p, err := workitem.PlaybookFor(ctx, tx, it)
	if err != nil {
		return nil, err
	}
	b := builder{ctx: ctx, tx: tx, p: p}
	if err := b.loadNames(); err != nil {
		return nil, err
	}
	if err := b.loadBatches(it.ID); err != nil {
		return nil, err
	}
	if err := b.addEvents(it.ID); err != nil {
		return nil, err
	}
	if err := b.addMessages(it.ID); err != nil {
		return nil, err
	}
	// Events came in ledger order and messages in sent order; a stable sort by time keeps that
	// order for entries recorded at the same moment.
	slices.SortStableFunc(b.entries, func(x, y Entry) int { return x.At.Compare(y.At) })
	return b.entries, nil
}

type builder struct {
	ctx     context.Context
	tx      pgx.Tx
	p       *playbook.Playbook
	names   map[string]string // person id → display name
	roles   map[string]string // role key → name, for hand-offs to a team
	numbers map[string]string // item id → number, for links
	before  engine.Fields     // the item's fields before the event being described
	batches map[string]int    // batch id → how many items that press moved (see batchSize)
	entries []Entry
}

func (b *builder) loadNames() error {
	rows, _ := b.tx.Query(b.ctx, `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant()`)
	pairs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Name string }])
	if err != nil {
		return fmt.Errorf("thread: people: %w", err)
	}
	b.names = map[string]string{}
	for _, p := range pairs {
		b.names[p.ID] = p.Name
	}
	rows, _ = b.tx.Query(b.ctx, `SELECT id::text, number FROM work_items WHERE tenant_id = current_tenant()`)
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Number string }])
	if err != nil {
		return fmt.Errorf("thread: items: %w", err)
	}
	b.numbers = map[string]string{}
	for _, it := range items {
		b.numbers[it.ID] = it.Number
	}
	rows, _ = b.tx.Query(b.ctx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	roles, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key, Name string }])
	if err != nil {
		return fmt.Errorf("thread: roles: %w", err)
	}
	b.roles = map[string]string{}
	for _, r := range roles {
		b.roles[r.Key] = r.Name
	}
	return nil
}

// eventsQuery reads one item's own events. It compares only subject_id, so Postgres can use the
// ledger_subject index instead of reading every event of the business. Link events (item.linked)
// are left out: starting linked work posts a message in both threads instead.
const eventsQuery = `
	SELECT seq, occurred_at, kind, actor_kind, coalesce(actor_person_id::text, ''),
		coalesce(on_behalf_of::text, ''), body
	FROM ledger
	WHERE tenant_id = current_tenant() AND subject_id = $1::uuid
		AND subject_type = 'work_item' AND kind <> 'item.linked'
	ORDER BY seq`

func (b *builder) addEvents(itemID string) error {
	rows, err := b.tx.Query(b.ctx, eventsQuery, itemID)
	if err != nil {
		return fmt.Errorf("thread: events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var actorKind, actor, behalf, body string
		if err := rows.Scan(&e.Seq, &e.At, &e.Kind, &actorKind, &actor, &behalf, &body); err != nil {
			return err
		}
		var parsed struct {
			Payload struct {
				Change map[string]any `json:"change"`
				Item   workitem.Item  `json:"item"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			return fmt.Errorf("thread: event %d: %w", e.Seq, err)
		}
		e.Who = b.who(actorKind, actor, behalf)
		pl := parsed.Payload
		e.Text = b.describe(e.Kind, pl.Change, pl.Item)
		b.before = pl.Item.Fields
		if e.Text != "" {
			b.entries = append(b.entries, e)
		}
	}
	return rows.Err()
}

func (b *builder) addMessages(itemID string) error {
	rows, err := b.tx.Query(b.ctx, `
		SELECT id::text FROM messages WHERE tenant_id = current_tenant() AND work_item_id::text = $1
		ORDER BY sent_at, id`, itemID)
	if err != nil {
		return fmt.Errorf("thread: messages: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		m, err := channel.Get(b.ctx, b.tx, id)
		if err != nil {
			return err
		}
		who := "Seyal"
		if m.AuthorKind == "person" {
			who = b.name(m.PersonID)
		}
		b.entries = append(b.entries, Entry{At: m.SentAt, Kind: "message", Who: who, Text: m.Text, Message: &m})
	}
	return nil
}

// describe turns one item event into plain words. It returns "" for events not shown in threads.
func (b *builder) describe(kind string, change map[string]any, it workitem.Item) string {
	str := func(key string) string { s, _ := change[key].(string); return s }
	switch kind {
	case "item.started":
		return "started " + it.Number
	case "item.fields_set":
		if change["copied_from"] != nil {
			return "" // Seyal keeping a value for later is bookkeeping, not news
		}
		values, _ := change["values"].(map[string]any)
		labels := make([]string, 0, len(values))
		for _, key := range slices.Sorted(maps.Keys(values)) {
			if !engine.SameValue(b.before[key], it.Fields[key]) {
				labels = append(labels, b.label(key))
			}
		}
		if len(labels) == 0 {
			return ""
		}
		text := "changed " + strings.Join(labels, ", ")
		if from := str("state_from"); from != "" {
			text += fmt.Sprintf(" (so it went from %s back to %s)", human(from), human(it.State))
		}
		return text
	case "item.state_changed":
		from, to := human(str("from")), human(str("to"))
		switch str("action") {
		case "approvals_complete":
			return fmt.Sprintf("all approvals are in: %s → %s", from, to)
		case "rejected":
			return fmt.Sprintf("an approval was refused: %s → %s", from, to)
		}
		text := fmt.Sprintf("%s: %s → %s", human(str("action")), from, to)
		if reason := str("reason"); reason != "" {
			text += ". Reason: " + reason
		}
		return text
	case "item.decided":
		verb := "approved"
		if str("decision") == "reject" {
			verb = "refused"
		}
		text := fmt.Sprintf("%s as %s", verb, human(str("slot")))
		if reason := str("reason"); reason != "" {
			text += ". Reason: " + reason
		}
		if steps, _ := change["worked_escalated"].([]any); len(steps) > 0 {
			// They did that step's work themselves after Seyal escalated it (D75, decision 11).
			names := make([]string, 0, len(steps))
			for _, st := range steps {
				if s, ok := st.(string); ok {
					names = append(names, capital(human(s)))
				}
			}
			text += ". Worked " + strings.Join(names, " and ") + " after it was escalated"
		}
		return text
	case "item.posted":
		return str("text")
	case "item.assigned":
		return b.handOver(str)
	}
	return ""
}

// handOver describes an item.assigned event by how it changed hands (D70). Events written before D70
// have no "how" and read as a step's owner getting it.
func (b *builder) handOver(str func(string) string) string {
	to := b.name(str("assignee_id"))
	if team := str("team"); team != "" {
		to = b.team(team)
	}
	var text string
	switch str("how") {
	case workitem.HowPass:
		text = "passed it to " + to
	case workitem.HowTake:
		text = "took it"
		if from := str("from_team"); from != "" {
			text += " from " + b.team(from)
		}
	case workitem.HowReturn:
		text = "gave it back to " + to // the reason is already in the move's own line
	case workitem.HowEscalate:
		text = "handed it to " + to + " (escalated)" // the note says how long it waited, and with whom
	case workitem.HowReassign:
		// "moved it from Ben to Dev (as an admin, with 2 other items)" (D75, M3).
		text = "moved it from " + b.name(str("from_holder")) + " to " + to + " (as an admin"
		switch others := b.batchSize(str("batch")) - 1; {
		case others == 1:
			text += ", with 1 other item"
		case others > 1:
			text += fmt.Sprintf(", with %d other items", others)
		}
		text += ")"
	default: // a step's owner, or an "assign" action of the process
		if str("team") != "" {
			text = "handed it to " + to
		} else {
			text = "assigned it to " + to
		}
	}
	if str("as") == "admin" && str("how") != workitem.HowReassign {
		text += " (as an admin)"
	}
	if note := str("note"); note != "" && str("how") != workitem.HowReturn {
		text += ": " + note
	}
	return text
}

// batchSize is how many items one press moved together (the hand-overs with this batch id), or 0.
func (b *builder) batchSize(batch string) int { return b.batches[batch] }

// loadBatches counts, for each batch id on the item's hand-overs, the hand-overs of the whole business
// with that id: how many items that one press moved (D75, M3). It runs before the events are read,
// since one connection can't run a query while it is still reading another.
//
// It first reads this item's own hand-overs (found fast by the ledger_subject index). Only an item that
// was moved with others then looks at the business's other hand-overs, so opening any other item never
// reads the business's whole history of hand-overs.
func (b *builder) loadBatches(itemID string) error {
	b.batches = map[string]int{}
	rows, _ := b.tx.Query(b.ctx, `
		SELECT DISTINCT body::jsonb #>> '{payload,change,batch}' FROM ledger
		WHERE tenant_id = current_tenant() AND subject_id = $1::uuid AND kind = 'item.assigned'
			AND body LIKE '%"batch":%'`, itemID)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[*string])
	if err != nil {
		return fmt.Errorf("thread: batches: %w", err)
	}
	var batches []string
	for _, id := range ids {
		if id != nil && *id != "" {
			batches = append(batches, *id)
		}
	}
	if len(batches) == 0 {
		return nil
	}
	rows, _ = b.tx.Query(b.ctx, `
		SELECT body::jsonb #>> '{payload,change,batch}' AS batch, count(*)::int FROM ledger
		WHERE tenant_id = current_tenant() AND kind = 'item.assigned' AND body LIKE '%"batch":%'
			AND body::jsonb #>> '{payload,change,batch}' = ANY($1)
		GROUP BY 1`, batches)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Batch string
		N     int
	}])
	if err != nil {
		return fmt.Errorf("thread: batches: %w", err)
	}
	for _, l := range list {
		b.batches[l.Batch] = l.N
	}
	return nil
}

// team names a role's team, e.g. "the Ops team".
func (b *builder) team(role string) string {
	return "the " + cmp.Or(b.roles[role], role) + " team"
}

// who names the actor of an event the way people expect to read it.
func (b *builder) who(actorKind, actor, behalf string) string {
	switch actorKind {
	case "human":
		return b.name(actor)
	case "agent":
		return "Seyal for " + b.name(behalf)
	}
	return "Seyal"
}

func (b *builder) name(personID string) string {
	if n, ok := b.names[personID]; ok {
		return n
	}
	return "someone"
}

func (b *builder) number(itemID string) string {
	if n, ok := b.numbers[itemID]; ok {
		return n
	}
	return "another item"
}

// label returns a field's label from the playbook, or a readable form of its key.
func (b *builder) label(key string) string {
	for _, f := range b.p.Fields {
		if f.Key == key && f.Label != "" {
			return f.Label
		}
	}
	return human(key)
}

// human makes a key like "pending_approval" readable: "pending approval".
func human(key string) string {
	return strings.ReplaceAll(key, "_", " ")
}

// capital starts a step's name with a capital letter: "with ops" → "With ops".
func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
