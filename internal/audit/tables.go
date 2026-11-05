// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit checks that the tables Seyal keeps for screens (work items, approvals, links,
// master items, lists, playbook versions, messages and voice transcripts) match what the ledger says happened. The ledger is the
// record; the tables are a convenient copy. Anyone who edits a table directly, bypassing Seyal,
// is caught here.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/workitem"
)

// Problem is one difference between a table and the ledger.
type Problem struct {
	Table  string
	ID     string
	Reason string
}

func (p Problem) String() string { return fmt.Sprintf("%s %s: %s", p.Table, p.ID, p.Reason) }

// CheckTables rebuilds the expected tables from the current tenant's ledger and compares them
// with the real ones. It trusts the ledger, so run ledger.Verify first.
// tx must come from db.WithTenant.
func CheckTables(ctx context.Context, tx pgx.Tx) ([]Problem, error) {
	want, err := replay(ctx, tx)
	if err != nil {
		return nil, err
	}
	c := &checker{ctx: ctx, tx: tx, want: want}
	for _, check := range []func() error{c.items, c.approvals, c.links, c.masters, c.kinds, c.playbooks, c.messages, c.transcripts} {
		if err := check(); err != nil {
			return nil, err
		}
	}
	return c.problems, nil
}

// expected is what the tables should contain, according to the ledger.
type expected struct {
	items     map[string]workitem.Item // by id
	approvals map[string]approval      // by "item/version/slot"
	links     map[string]bool          // by "from/to/kind"
	masters   map[string]masterRow     // by id
	kinds     map[string]kindRow       // lists, by id
	playbooks map[string]string        // id → sha256 of the file, in hex
	messages  map[string]channel.Message
	// transcripts are the voice notes' words that messages in the ledger were sent with, by id
	transcripts map[string]bool
}

type approval struct {
	PersonID   string
	Decision   string `json:"decision"`
	FieldsHash string `json:"fields_sha256"`
	Reason     string `json:"reason"`
}

type kindRow struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

type masterRow struct {
	Kind    string        `json:"kind"`
	Key     string        `json:"key"`
	Name    string        `json:"name"`
	Fields  engine.Fields `json:"fields"`
	Version int           `json:"version"`
	Active  bool          `json:"active"`
}

// replay reads the ledger from the first event to the last and builds the expected tables.
func replay(ctx context.Context, tx pgx.Tx) (expected, error) {
	want := expected{
		items:       map[string]workitem.Item{},
		approvals:   map[string]approval{},
		links:       map[string]bool{},
		masters:     map[string]masterRow{},
		kinds:       map[string]kindRow{},
		playbooks:   map[string]string{},
		messages:    map[string]channel.Message{},
		transcripts: map[string]bool{},
	}
	rows, err := tx.Query(ctx, `SELECT body FROM ledger WHERE tenant_id = current_tenant() ORDER BY seq`)
	if err != nil {
		return want, err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return want, err
		}
		var e struct {
			Kind      string          `json:"kind"`
			SubjectID string          `json:"subject_id"`
			Actor     string          `json:"actor"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return want, fmt.Errorf("audit: event body: %w", err)
		}
		if err := want.apply(e.Kind, e.SubjectID, e.Actor, e.Payload); err != nil {
			return want, fmt.Errorf("audit: %s event for %s: %w", e.Kind, e.SubjectID, err)
		}
	}
	return want, rows.Err()
}

// apply adds one event to the expected tables.
func (w expected) apply(kind, subject, actor string, payload json.RawMessage) error {
	switch kind {
	case "item.linked":
		var l struct{ From, To, Kind string }
		if err := json.Unmarshal(payload, &l); err != nil {
			return err
		}
		w.links[l.From+"/"+l.To+"/"+l.Kind] = true
		return nil
	case "master.changed", "master.deactivated":
		var m masterRow
		if err := json.Unmarshal(payload, &m); err != nil {
			return err
		}
		w.masters[subject] = m
		return nil
	case "master_kind.changed":
		var k kindRow
		if err := json.Unmarshal(payload, &k); err != nil {
			return err
		}
		w.kinds[subject] = k
		return nil
	case "playbook.registered":
		var p struct {
			SHA256 string `json:"sha256"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		w.playbooks[subject] = p.SHA256
		return nil
	case "message.received", "message.sent", "message.filed":
		var p struct {
			Message channel.Message `json:"message"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		w.messages[subject] = p.Message
		if t := p.Message.Transcript; t != nil {
			w.transcripts[t.ID] = true
		}
		return nil
	}
	// People and roles (logins, links, identities) aren't in the tables checked here; skipping them
	// by name means an odd payload (not an object, or with an "item" key) can't upset the replay.
	if strings.HasPrefix(kind, "person.") || strings.HasPrefix(kind, "role.") {
		return nil
	}

	// Every other item event carries a full snapshot of the item; the last one wins.
	var p struct {
		Change json.RawMessage `json:"change"`
		Item   *workitem.Item  `json:"item"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	if p.Item != nil {
		w.items[subject] = *p.Item
	}
	if kind == "item.decided" {
		var d struct {
			approval
			Slot        string `json:"slot"`
			ItemVersion int    `json:"item_version"`
		}
		if err := json.Unmarshal(p.Change, &d); err != nil {
			return err
		}
		d.PersonID = actor
		w.approvals[fmt.Sprintf("%s/%d/%s", subject, d.ItemVersion, d.Slot)] = d.approval
	}
	return nil
}

type checker struct {
	ctx      context.Context
	tx       pgx.Tx
	want     expected
	problems []Problem
}

func (c *checker) add(table, id, format string, args ...any) {
	c.problems = append(c.problems, Problem{Table: table, ID: id, Reason: fmt.Sprintf(format, args...)})
}

// leftovers reports everything the ledger expects that the table didn't have.
func leftovers[V any](c *checker, table string, want map[string]V) {
	for _, id := range slices.Sorted(maps.Keys(want)) {
		c.add(table, id, "missing from the table (removed outside Seyal)")
	}
}

func (c *checker) items() error {
	rows, err := c.tx.Query(c.ctx, `
		SELECT id::text, number, playbook_version_id::text, title, state,
			coalesce(assignee_id::text, ''), coalesce(to_char(due_on, 'YYYY-MM-DD'), ''),
			version, fields, fields_sha256, started_via, coalesce(started_ref, ''),
			coalesce(created_by::text, ''), coalesce(team, '')
		FROM work_items WHERE tenant_id = current_tenant() ORDER BY id`)
	if err != nil {
		return err
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[workitem.Item])
	if err != nil {
		return err
	}
	for _, g := range got {
		w, ok := c.want.items[g.ID]
		delete(c.want.items, g.ID)
		if !ok {
			c.add("work_items", g.ID, "not in the ledger (added outside Seyal)")
			continue
		}
		if reason := diffItem(w, g); reason != "" {
			c.add("work_items", g.ID, "%s", reason)
		}
	}
	leftovers(c, "work_items", c.want.items)
	return nil
}

// diffItem names the first difference between what the ledger says and what the table has.
func diffItem(w, g workitem.Item) string {
	wantHash, err1 := engine.HashFields(w.Fields)
	gotHash, err2 := engine.HashFields(g.Fields)
	checks := []struct {
		what      string
		want, got any
	}{
		{"number", w.Number, g.Number},
		{"playbook version", w.PlaybookVersionID, g.PlaybookVersionID},
		{"title", w.Title, g.Title},
		{"state", w.State, g.State},
		{"assignee", w.AssigneeID, g.AssigneeID},
		{"due date", w.DueOn, g.DueOn},
		{"version", w.Version, g.Version},
		{"fields", wantHash, gotHash},
		{"fields fingerprint", w.FieldsHash, g.FieldsHash},
		{"started via", w.StartedVia, g.StartedVia},
		{"started ref", w.StartedRef, g.StartedRef},
		{"created by", w.CreatedBy, g.CreatedBy},
		{"team", w.Team, g.Team},
	}
	if err1 != nil || err2 != nil {
		return "fields can't be read"
	}
	for _, ch := range checks {
		if ch.want != ch.got {
			if ch.what == "fields" {
				return "fields differ from the ledger"
			}
			return fmt.Sprintf("%s is %v, but the ledger says %v", ch.what, ch.got, ch.want)
		}
	}
	return ""
}

func (c *checker) approvals() error {
	rows, err := c.tx.Query(c.ctx, `
		SELECT work_item_id::text, item_version, slot, person_id::text, decision, fields_sha256,
			coalesce(reason, '')
		FROM approvals WHERE tenant_id = current_tenant() ORDER BY work_item_id, item_version, slot`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item, slot string
		var version int
		var g approval
		if err := rows.Scan(&item, &version, &slot, &g.PersonID, &g.Decision, &g.FieldsHash, &g.Reason); err != nil {
			return err
		}
		key := fmt.Sprintf("%s/%d/%s", item, version, slot)
		w, ok := c.want.approvals[key]
		delete(c.want.approvals, key)
		switch {
		case !ok:
			c.add("approvals", key, "not in the ledger (added outside Seyal)")
		case w != g:
			c.add("approvals", key, "differs from the ledger")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	leftovers(c, "approvals", c.want.approvals)
	return nil
}

func (c *checker) links() error {
	rows, err := c.tx.Query(c.ctx, `
		SELECT from_item_id::text || '/' || to_item_id::text || '/' || kind
		FROM work_item_links WHERE tenant_id = current_tenant() ORDER BY 1`)
	if err != nil {
		return err
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, key := range got {
		if !c.want.links[key] {
			c.add("work_item_links", key, "not in the ledger (added outside Seyal)")
		}
		delete(c.want.links, key)
	}
	leftovers(c, "work_item_links", c.want.links)
	return nil
}

func (c *checker) masters() error {
	rows, err := c.tx.Query(c.ctx, `
		SELECT id::text, kind, key, name, fields, version, active
		FROM master_items WHERE tenant_id = current_tenant() ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var g masterRow
		if err := rows.Scan(&id, &g.Kind, &g.Key, &g.Name, &g.Fields, &g.Version, &g.Active); err != nil {
			return err
		}
		w, ok := c.want.masters[id]
		delete(c.want.masters, id)
		if !ok {
			c.add("master_items", id, "not in the ledger (added outside Seyal)")
			continue
		}
		wantHash, _ := engine.HashFields(w.Fields)
		gotHash, _ := engine.HashFields(g.Fields)
		if w.Kind != g.Kind || w.Key != g.Key || w.Name != g.Name || wantHash != gotHash ||
			w.Version != g.Version || w.Active != g.Active {
			c.add("master_items", id, "%s %q differs from the ledger", g.Kind, g.Key)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	leftovers(c, "master_items", c.want.masters)
	return nil
}

func (c *checker) kinds() error {
	rows, err := c.tx.Query(c.ctx, `
		SELECT id::text, kind, label, version, content_sha256, source FROM master_kinds
		WHERE tenant_id = current_tenant() ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, source string
		var g kindRow
		var stored []byte
		if err := rows.Scan(&id, &g.Kind, &g.Label, &g.Version, &stored, &source); err != nil {
			return err
		}
		w, ok := c.want.kinds[id]
		delete(c.want.kinds, id)
		sum := sha256.Sum256([]byte(source))
		g.SHA256 = hex.EncodeToString(stored)
		switch {
		case !ok:
			c.add("master_kinds", id, "not in the ledger (added outside Seyal)")
		case w != g:
			c.add("master_kinds", id, "list %q differs from the ledger", g.Kind)
		case hex.EncodeToString(sum[:]) != w.SHA256:
			c.add("master_kinds", id, "the definition of list %q was changed outside Seyal", g.Kind)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	leftovers(c, "master_kinds", c.want.kinds)
	return nil
}

func (c *checker) playbooks() error {
	rows, err := c.tx.Query(c.ctx, `
		SELECT id::text, content_sha256, source FROM playbook_versions
		WHERE tenant_id = current_tenant() ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, source string
		var stored []byte
		if err := rows.Scan(&id, &stored, &source); err != nil {
			return err
		}
		w, ok := c.want.playbooks[id]
		delete(c.want.playbooks, id)
		sum := sha256.Sum256([]byte(source))
		switch {
		case !ok:
			c.add("playbook_versions", id, "not in the ledger (added outside Seyal)")
		case hex.EncodeToString(stored) != w:
			c.add("playbook_versions", id, "stored fingerprint differs from the ledger")
		case hex.EncodeToString(sum[:]) != w:
			c.add("playbook_versions", id, "the playbook text was changed after it was registered")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	leftovers(c, "playbook_versions", c.want.playbooks)
	return nil
}

func (c *checker) messages() error {
	got, err := channel.List(c.ctx, c.tx)
	if err != nil {
		return err
	}
	for _, g := range got {
		w, ok := c.want.messages[g.ID]
		delete(c.want.messages, g.ID)
		switch {
		case !ok:
			c.add("messages", g.ID, "not in the ledger (added outside Seyal)")
		case w.Text != g.Text:
			c.add("messages", g.ID, "text differs from the ledger")
		case w.ThreadID != g.ThreadID:
			c.add("messages", g.ID, "thread is %q, but the ledger says %q", g.ThreadID, w.ThreadID)
		case w.AuthorKind != g.AuthorKind || w.PersonID != g.PersonID || w.Channel != g.Channel ||
			w.ExternalID != g.ExternalID || !w.SentAt.Equal(g.SentAt):
			c.add("messages", g.ID, "sender, channel or time differs from the ledger")
		case !slices.Equal(w.Media, g.Media):
			c.add("messages", g.ID, "attached files differ from the ledger")
		case (w.Transcript == nil) != (g.Transcript == nil) || w.Transcript != nil && *w.Transcript != *g.Transcript:
			c.add("messages", g.ID, "the voice note's words differ from the ledger")
		}
	}
	leftovers(c, "messages", c.want.messages)
	return nil
}

// transcripts checks that every transcript belongs to a message in the ledger. What a transcript
// says (the words heard, the recording) is checked with its message, in messages: the message's
// ledger event holds it. tx must come from db.WithTenant.
func (c *checker) transcripts() error {
	ids, err := channel.TranscriptIDs(c.ctx, c.tx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !c.want.transcripts[id] {
			c.add("transcripts", id, "not in the ledger (added outside Seyal)")
		}
	}
	return nil
}
