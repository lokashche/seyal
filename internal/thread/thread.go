// SPDX-License-Identifier: AGPL-3.0-or-later

// Package thread builds a work item's timeline: its messages and voice notes, and everything that
// happened to it, in time order. Events are described in plain words using the item's own
// playbook (field labels, state names), so the engine itself stays free of business words.
package thread

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/channel"
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
	numbers map[string]string // item id → number, for links
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
	return nil
}

func (b *builder) addEvents(itemID string) error {
	rows, err := b.tx.Query(b.ctx, `
		SELECT seq, occurred_at, kind, actor_kind, coalesce(actor_person_id::text, ''),
			coalesce(on_behalf_of::text, ''), body
		FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item'
			AND (subject_id::text = $1 OR (kind = 'item.linked' AND body::jsonb -> 'payload' ->> 'to' = $1))
		ORDER BY seq`, itemID)
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
				// item.linked has these instead
				From, To, Kind string
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			return fmt.Errorf("thread: event %d: %w", e.Seq, err)
		}
		e.Who = b.who(actorKind, actor, behalf)
		pl := parsed.Payload
		if e.Kind == "item.linked" {
			e.Text = b.describeLink(itemID, pl.From, pl.To, pl.Kind)
		} else {
			e.Text = b.describe(e.Kind, pl.Change, pl.Item)
		}
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
		values, _ := change["values"].(map[string]any)
		labels := make([]string, 0, len(values))
		for _, key := range slices.Sorted(maps.Keys(values)) {
			labels = append(labels, b.label(key))
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
		return text
	case "item.posted":
		return str("text")
	case "item.assigned":
		return "assigned it to " + b.name(str("assignee_id"))
	}
	return ""
}

func (b *builder) describeLink(itemID, from, to, kind string) string {
	switch {
	case kind == "parent" && from == itemID:
		return "added sub-item " + b.number(to)
	case kind == "parent":
		return "made this a sub-item of " + b.number(from)
	case kind == "started_by" && from == itemID:
		return "started because of " + b.number(to)
	default:
		return "started " + b.number(from) + " from this"
	}
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
