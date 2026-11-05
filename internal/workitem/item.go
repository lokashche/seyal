// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	_ "time/tzdata" // time zone data built in, so tenant time zones work on any server

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
)

// Item is a stored work item. It is also the snapshot written into every ledger event about
// the item, so the ledger alone can rebuild the work_items table.
type Item struct {
	ID                string        `json:"id"`
	Number            string        `json:"number"`
	PlaybookVersionID string        `json:"playbook_version_id"`
	Title             string        `json:"title"`
	State             string        `json:"state"`
	AssigneeID        string        `json:"assignee_id,omitempty"`
	DueOn             string        `json:"due_on,omitempty"` // e.g. "2026-11-15"
	Version           int           `json:"version"`
	Fields            engine.Fields `json:"fields"`
	FieldsHash        string        `json:"fields_sha256"`
	StartedVia        string        `json:"started_via"`
	StartedRef        string        `json:"started_ref,omitempty"`
	CreatedBy         string        `json:"created_by,omitempty"`
	Team              string        `json:"team,omitempty"` // a role that has the item, while no one person does
}

// ErrNotFound means there is no work item with that id.
var ErrNotFound = errors.New("work item not found")

const selectItems = `
	SELECT id::text, number, playbook_version_id::text, title, state,
		coalesce(assignee_id::text, ''), coalesce(to_char(due_on, 'YYYY-MM-DD'), ''),
		version, fields, fields_sha256, started_via, coalesce(started_ref, ''),
		coalesce(created_by::text, ''), coalesce(team, '')
	FROM work_items WHERE tenant_id = current_tenant()`

const selectItem = selectItems + ` AND id::text = $1`

// Get returns one work item. tx must come from db.WithTenant.
func Get(ctx context.Context, tx pgx.Tx, id string) (Item, error) {
	return scanItem(tx.QueryRow(ctx, selectItem, id))
}

// List returns the tenant's open work items (all of them: nothing open may go missing), most recently
// changed first. Which states are closed depends on each item's process version.
// tx must come from db.WithTenant.
func List(ctx context.Context, tx pgx.Tx) ([]Item, error) {
	rows, _ := tx.Query(ctx, `
		SELECT DISTINCT v.id::text, v.source FROM work_items w
		JOIN playbook_versions v ON v.tenant_id = w.tenant_id AND v.id = w.playbook_version_id
		WHERE w.tenant_id = current_tenant()`)
	versions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Source string }])
	if err != nil {
		return nil, fmt.Errorf("workitem: list: %w", err)
	}
	var closed []string // "version id:state"
	for _, v := range versions {
		p, err := playbook.Parse([]byte(v.Source))
		if err != nil {
			return nil, fmt.Errorf("workitem: list: %w", err)
		}
		for _, st := range p.States {
			if engine.IsClosed(p, st) {
				closed = append(closed, v.ID+":"+st)
			}
		}
	}
	rows, err = tx.Query(ctx, selectItems+`
		AND (playbook_version_id::text || ':' || state) <> ALL($1::text[])
		ORDER BY updated_at DESC, number`, closed)
	if err != nil {
		return nil, fmt.Errorf("workitem: list: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// All returns every work item of the tenant, open and closed, most recently started first.
// tx must come from db.WithTenant.
func All(ctx context.Context, tx pgx.Tx) ([]Item, error) {
	rows, err := tx.Query(ctx, selectItems+` ORDER BY created_at DESC, number`)
	if err != nil {
		return nil, fmt.Errorf("workitem: all: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// getForUpdate returns one work item and locks it until the transaction ends, so two people
// changing the same item at once take turns. The ledger is locked first (see ledger.Lock).
func getForUpdate(ctx context.Context, tx pgx.Tx, id string) (Item, error) {
	if err := ledger.Lock(ctx, tx); err != nil {
		return Item{}, err
	}
	return scanItem(tx.QueryRow(ctx, selectItem+" FOR UPDATE", id))
}

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.Number, &it.PlaybookVersionID, &it.Title, &it.State, &it.AssigneeID,
		&it.DueOn, &it.Version, &it.Fields, &it.FieldsHash, &it.StartedVia, &it.StartedRef, &it.CreatedBy, &it.Team)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("workitem: read: %w", err)
	}
	if it.Fields == nil {
		it.Fields = engine.Fields{}
	}
	return it, nil
}

// PlaybookFor returns the exact playbook version the item follows. tx must come from db.WithTenant.
func PlaybookFor(ctx context.Context, tx pgx.Tx, it Item) (*playbook.Playbook, error) {
	s, err := open(ctx, tx, it.PlaybookVersionID)
	return s.p, err
}

// session bundles what every action needs: the transaction and the item's own playbook.
type session struct {
	ctx context.Context
	tx  pgx.Tx
	p   *playbook.Playbook
}

// open loads the exact playbook version an item is pinned to.
func open(ctx context.Context, tx pgx.Tx, versionID string) (session, error) {
	var source string
	err := tx.QueryRow(ctx, `
		SELECT source FROM playbook_versions WHERE tenant_id = current_tenant() AND id::text = $1`,
		versionID).Scan(&source)
	if err != nil {
		return session{}, fmt.Errorf("workitem: playbook version %s: %w", versionID, err)
	}
	p, err := playbook.Parse([]byte(source))
	if err != nil {
		return session{}, err
	}
	if err := p.Validate(); err != nil {
		return session{}, err
	}
	return session{ctx: ctx, tx: tx, p: p}, nil
}

func (s session) insert(it *Item) error {
	err := s.tx.QueryRow(s.ctx, `
		INSERT INTO work_items (tenant_id, number, playbook_version_id, title, state, assignee_id, due_on,
			version, fields, fields_sha256, started_via, started_ref, created_by, team)
		VALUES (current_tenant(), $1, $2, $3, $4, nullif($5::text, '')::uuid, nullif($6::text, '')::date,
			$7, $8, $9, $10, nullif($11::text, ''), nullif($12::text, '')::uuid, nullif($13::text, ''))
		RETURNING id::text`,
		it.Number, it.PlaybookVersionID, it.Title, it.State, it.AssigneeID, it.DueOn,
		it.Version, it.Fields, it.FieldsHash, it.StartedVia, it.StartedRef, it.CreatedBy, it.Team).Scan(&it.ID)
	if err != nil {
		return fmt.Errorf("workitem: insert: %w", err)
	}
	return nil
}

func (s session) update(it Item) error {
	_, err := s.tx.Exec(s.ctx, `
		UPDATE work_items SET title = $2, state = $3, assignee_id = nullif($4::text, '')::uuid,
			due_on = nullif($5::text, '')::date, version = $6, fields = $7, fields_sha256 = $8,
			team = nullif($9::text, ''), updated_at = now()
		WHERE tenant_id = current_tenant() AND id::text = $1`,
		it.ID, it.Title, it.State, it.AssigneeID, it.DueOn, it.Version, it.Fields, it.FieldsHash, it.Team)
	if err != nil {
		return fmt.Errorf("workitem: update: %w", err)
	}
	return nil
}

// record writes an event about the item to the ledger: what changed, plus a full snapshot.
func (s session) record(kind string, it Item, change map[string]any, by ledger.Actor) error {
	_, err := ledger.Append(s.ctx, s.tx, ledger.Event{
		Kind:        kind,
		SubjectType: "work_item",
		SubjectID:   it.ID,
		Payload:     map[string]any{"change": change, "item": it},
	}.By(by))
	return err
}

// refresh recomputes what is derived from the fields: fingerprint, title and due date.
func (s session) refresh(it *Item) error {
	hash, err := engine.HashFields(it.Fields)
	if err != nil {
		return err
	}
	it.FieldsHash = hash
	it.DueOn = s.due(it.Fields)
	it.Title, err = s.render(s.p.Title, it.Fields)
	return err
}

// due returns the earliest date among the playbook's deadline fields, or "".
func (s session) due(fields engine.Fields) string {
	earliest := ""
	for _, d := range s.p.Deadlines {
		if date, ok := fields[d.DueFrom].(string); ok && date != "" && (earliest == "" || date < earliest) {
			earliest = date
		}
	}
	return earliest
}

var placeholder = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

// render fills a text such as "Approved. Over to {maker}." with readable field values.
func (s session) render(text string, fields engine.Fields) (string, error) {
	var firstErr error
	out := placeholder.ReplaceAllStringFunc(text, func(token string) string {
		key := token[1 : len(token)-1]
		f := s.field(key)
		if f == nil {
			return token
		}
		shown, err := s.display(*f, fields[key])
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return shown
	})
	return strings.TrimSpace(out), firstErr
}

// display turns a field value into text people can read: names for people and master items.
func (s session) display(f playbook.Field, v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			shown, err := s.display(playbook.Field{Key: f.Key, Type: f.Of}, item)
			if err != nil {
				return "", err
			}
			parts = append(parts, shown)
		}
		return strings.Join(parts, ", "), nil
	case string:
		switch f.Type {
		case "person":
			return s.personName(x)
		case "master":
			m, err := master.Get(s.ctx, s.tx, f.Kind, x)
			if errors.Is(err, master.ErrNotFound) {
				return x, nil
			}
			return m.Name, err
		}
		return x, nil
	case float64:
		if f.Type == "money" {
			return strconv.FormatFloat(x/100, 'f', 2, 64), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case bool:
		if x {
			return "yes", nil
		}
		return "no", nil
	}
	return fmt.Sprint(v), nil
}

func (s session) personName(id string) (string, error) {
	var name string
	err := s.tx.QueryRow(s.ctx, `
		SELECT display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, nil
	}
	return name, err
}

// checkPerson makes sure id is an active person of the current tenant.
func (s session) checkPerson(key, id string) error {
	var ok bool
	err := s.tx.QueryRow(s.ctx, `
		SELECT EXISTS (SELECT 1 FROM persons
			WHERE tenant_id = current_tenant() AND id::text = $1 AND status = 'active')`, id).Scan(&ok)
	if err == nil && !ok {
		err = fmt.Errorf("%s: unknown person", key)
	}
	return err
}

func (s session) field(key string) *playbook.Field {
	for i := range s.p.Fields {
		if s.p.Fields[i].Key == key {
			return &s.p.Fields[i]
		}
	}
	return nil
}

func (s session) engineItem(it Item) engine.Item { return it.Engine() }

// Engine is the part of the item the engine's rules need: its state, fields and who has it.
func (it Item) Engine() engine.Item {
	return engine.Item{State: it.State, Version: it.Version, Fields: it.Fields,
		Assignee: it.AssigneeID, Team: it.Team, Starter: it.CreatedBy}
}

// actorPerson is the person behind an action: the person themselves, or whom the agent acted for.
func actorPerson(by ledger.Actor) string {
	if by.PersonID != "" {
		return by.PersonID
	}
	return by.OnBehalfOf
}
