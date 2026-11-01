// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
)

// NewItem describes a work item to start.
type NewItem struct {
	PlaybookVersionID string
	Via               string // chat, schedule or event
	Ref               string // what started it, e.g. a message id
	Fields            engine.Fields
	Parent            string // optional: id of the parent item
	StartedBy         string // optional: id of the item whose change started this one
	By                ledger.Actor
}

// Start creates a work item: it takes the next number, fills the given fields (and whatever
// they pre-fill), and records item.started. tx must come from db.WithTenant.
func Start(ctx context.Context, tx pgx.Tx, in NewItem) (Item, error) {
	s, err := open(ctx, tx, in.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}

	var zone string
	var fiscalStart int
	if err := tx.QueryRow(ctx, `
		SELECT timezone, fiscal_year_start_month FROM tenants WHERE id = current_tenant()`).
		Scan(&zone, &fiscalStart); err != nil {
		return Item{}, fmt.Errorf("workitem: tenant settings: %w", err)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return Item{}, fmt.Errorf("workitem: tenant time zone: %w", err)
	}
	year := FiscalYear(time.Now().In(loc), fiscalStart)
	seq, err := TakeNumber(ctx, tx, fmt.Sprintf("%s:%d", s.p.ID, year))
	if err != nil {
		return Item{}, err
	}

	it := Item{
		Number:            FormatNumber(s.p.Numbering, year, seq),
		PlaybookVersionID: in.PlaybookVersionID,
		State:             s.p.Initial,
		Version:           1,
		Fields:            engine.Fields{},
		StartedVia:        in.Via,
		StartedRef:        in.Ref,
		CreatedBy:         actorPerson(in.By),
	}
	if err := s.apply(&it, in.Fields); err != nil {
		return Item{}, err
	}
	if err := s.refresh(&it); err != nil {
		return Item{}, err
	}
	if err := s.insert(&it); err != nil {
		return Item{}, err
	}
	if err := s.record("item.started", it, map[string]any{"via": in.Via, "ref": in.Ref}, in.By); err != nil {
		return Item{}, err
	}
	if in.Parent != "" {
		if err := Link(ctx, tx, in.Parent, it.ID, "parent", in.By); err != nil {
			return Item{}, err
		}
	}
	if in.StartedBy != "" {
		if err := Link(ctx, tx, it.ID, in.StartedBy, "started_by", in.By); err != nil {
			return Item{}, err
		}
	}
	if err := s.enter(&it); err != nil {
		return Item{}, err
	}
	return it, nil
}

// SetFields changes fields of an item, as a person (or an agent for a person) would. If anything
// really changed, the version goes up, so earlier approvals stop counting, and an approved item
// goes back to waiting for approvals. tx must come from db.WithTenant.
func SetFields(ctx context.Context, tx pgx.Tx, id string, values engine.Fields, by ledger.Actor) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	before := it.FieldsHash
	if err := s.apply(&it, values); err != nil {
		return Item{}, err
	}
	if err := s.refresh(&it); err != nil {
		return Item{}, err
	}
	if it.FieldsHash == before {
		return it, nil // nothing really changed
	}

	it.Version++
	change := map[string]any{"values": values}
	from := it.State
	it.State = engine.StateAfterEdit(s.p, it.State)
	if it.State != from {
		change["state_from"] = from
	}
	if err := s.update(it); err != nil {
		return Item{}, err
	}
	if err := s.record("item.fields_set", it, change, by); err != nil {
		return Item{}, err
	}
	if it.State != from {
		if err := s.enter(&it); err != nil {
			return Item{}, err
		}
	}
	return it, nil
}

// Act performs one of the playbook's actions, such as submit or cancel.
// tx must come from db.WithTenant.
func Act(ctx context.Context, tx pgx.Tx, id, action, reason string, by ledger.Actor) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	lookup, err := master.LookupFor(ctx, tx, s.p, it.Fields)
	if err != nil {
		return Item{}, err
	}
	to, err := engine.Transition(s.p, s.engineItem(it), action, reason, lookup)
	if err != nil {
		return Item{}, err
	}
	change := map[string]any{"action": action}
	if reason != "" {
		change["reason"] = reason
	}
	if err := s.move(&it, to, change, by); err != nil {
		return Item{}, err
	}
	return it, nil
}

// Decide records a person's approval or rejection of one approval slot. Only people (not agents)
// decide, and the engine's rules apply: the right role, one decision per slot and per person for
// each version, and a reason for a rejection. tx must come from db.WithTenant.
func Decide(ctx context.Context, tx pgx.Tx, id, personID, slot, decision, reason string) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	rows, _ := tx.Query(ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL`, personID)
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Item{}, fmt.Errorf("workitem: roles: %w", err)
	}
	given, lookup, err := s.approvalInputs(it)
	if err != nil {
		return Item{}, err
	}
	who := engine.Person{ID: personID, Roles: roles}
	if err := engine.CheckDecision(s.p, s.engineItem(it), who, slot, decision, reason, given, lookup); err != nil {
		return Item{}, err
	}

	hash, err := engine.HashFields(it.Fields)
	if err != nil {
		return Item{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO approvals (tenant_id, work_item_id, item_version, fields_sha256, slot, person_id, decision, reason)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, nullif($7::text, ''))`,
		it.ID, it.Version, hash, slot, personID, decision, reason); err != nil {
		return Item{}, fmt.Errorf("workitem: save decision: %w", err)
	}
	change := map[string]any{"slot": slot, "decision": decision, "item_version": it.Version, "fields_sha256": hash}
	if reason != "" {
		change["reason"] = reason
	}
	if err := s.record("item.decided", it, change, ledger.PersonActor(personID)); err != nil {
		return Item{}, err
	}
	if err := s.settle(&it); err != nil {
		return Item{}, err
	}
	return it, nil
}

// Approvals returns the approval status of the item's current version, for screens.
// tx must come from db.WithTenant.
func Approvals(ctx context.Context, tx pgx.Tx, id string) (engine.ApprovalStatus, error) {
	it, err := Get(ctx, tx, id)
	if err != nil {
		return engine.ApprovalStatus{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return engine.ApprovalStatus{}, err
	}
	return s.status(it)
}

// Link connects two items. kind is "parent" (from = parent, to = child) or "started_by"
// (from = the new item, to = the item that started it). tx must come from db.WithTenant.
func Link(ctx context.Context, tx pgx.Tx, fromID, toID, kind string, by ledger.Actor) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO work_item_links (tenant_id, from_item_id, to_item_id, kind, created_by)
		VALUES (current_tenant(), $1, $2, $3, nullif($4::text, '')::uuid)`,
		fromID, toID, kind, actorPerson(by)); err != nil {
		return fmt.Errorf("workitem: link: %w", err)
	}
	_, err := ledger.Append(ctx, tx, ledger.Event{
		Kind:        "item.linked",
		SubjectType: "work_item",
		SubjectID:   fromID,
		Payload:     map[string]string{"from": fromID, "to": toID, "kind": kind},
	}.By(by))
	return err
}

// apply sets fields on it, checking each value. Master fields go first, so their pre-fill
// happens before the other values, which then win over pre-filled ones.
func (s session) apply(it *Item, values engine.Fields) error {
	keys := slices.SortedFunc(maps.Keys(values), func(a, b string) int {
		aMaster, bMaster := s.isMaster(a), s.isMaster(b)
		switch {
		case aMaster && !bMaster:
			return -1
		case bMaster && !aMaster:
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, key := range keys {
		v := values[key]
		if err := engine.CheckSet(s.p, s.engineItem(*it), key, v); err != nil {
			return err
		}
		if v == nil {
			delete(it.Fields, key)
			continue
		}
		f := s.field(key)
		switch {
		case f.Type == "person":
			if err := s.checkPerson(key, v.(string)); err != nil {
				return err
			}
		case f.Type == "list" && f.Of == "person":
			for _, id := range v.([]any) {
				if err := s.checkPerson(key, id.(string)); err != nil {
					return err
				}
			}
		}
		it.Fields[key] = v
		if f.Type == "master" {
			if err := s.prefill(it, *f, v.(string)); err != nil {
				return err
			}
		}
	}
	return nil
}

// prefill copies values from the chosen master item into the fields its playbook field names.
// A value the master item doesn't have clears the field, so nothing stale is left behind.
func (s session) prefill(it *Item, f playbook.Field, key string) error {
	m, err := master.Get(s.ctx, s.tx, f.Kind, key)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Key, err)
	}
	if !m.Active {
		return fmt.Errorf("%s: %s %q is no longer in use", f.Key, f.Kind, key)
	}
	for target, from := range f.Prefill {
		if v, ok := m.Fields[from]; ok {
			it.Fields[target] = v
		} else {
			delete(it.Fields, target)
		}
	}
	return nil
}

// move puts the item into a new state, records it, and runs what happens on entering it.
// Entering the approval state starts a new approval round: the version goes up, so decisions
// from an earlier round (e.g. a rejection) don't count again.
func (s session) move(it *Item, to string, change map[string]any, by ledger.Actor) error {
	from := it.State
	it.State = to
	if a := s.p.Approvals; a != nil && to == a.In && from != a.In {
		it.Version++
	}
	change["from"], change["to"] = from, to
	if err := s.update(*it); err != nil {
		return err
	}
	if err := s.record("item.state_changed", *it, change, by); err != nil {
		return err
	}
	return s.enter(it)
}

// enter hands the item to the owner of its new step, runs the playbook's actions for the state, then
// follows any automatic move (all approvals given, or one rejected).
func (s session) enter(it *Item) error {
	if err := s.handOver(it); err != nil {
		return err
	}
	for _, a := range s.p.On[it.State] {
		switch a.Do {
		case "copy":
			if err := s.copyField(it, a.From, a.To); err != nil {
				return err
			}
		case "post":
			text, err := s.render(a.Text, it.Fields)
			if err != nil {
				return err
			}
			if err := s.record("item.posted", *it, map[string]any{"text": text}, ledger.SystemActor); err != nil {
				return err
			}
		case "assign":
			id, _ := it.Fields[a.To].(string)
			if id == "" {
				continue
			}
			if err := s.assign(it, id, ""); err != nil {
				return err
			}
		}
	}
	return s.settle(it)
}

// handOver gives the item to the owner of its current step, if the step names one: whoever started
// it, a person field, or a role (anyone in it, shown as the item's team).
func (s session) handOver(it *Item) error {
	kind, name := engine.Owner(s.p, it.State)
	switch kind {
	case "starter":
		if it.CreatedBy != "" {
			return s.assign(it, it.CreatedBy, "")
		}
	case "field":
		if id, _ := it.Fields[name].(string); id != "" {
			return s.assign(it, id, "")
		}
	case "role":
		return s.assign(it, "", name)
	}
	return nil
}

// assign gives the item to one person, or to a team (a role) when person is "".
func (s session) assign(it *Item, person, team string) error {
	if it.AssigneeID == person && it.Team == team {
		return nil
	}
	it.AssigneeID, it.Team = person, team
	if err := s.update(*it); err != nil {
		return err
	}
	change := map[string]any{"assignee_id": person}
	if team != "" {
		change["team"] = team
	}
	return s.record("item.assigned", *it, change, ledger.SystemActor)
}

// copyField copies one field's value into another (e.g. keeping a proposed amount to compare with
// later). A change is recorded like any edit, by Seyal.
func (s session) copyField(it *Item, from, to string) error {
	v := it.Fields[from]
	if engine.SameValue(v, it.Fields[to]) {
		return nil
	}
	if v == nil {
		delete(it.Fields, to)
	} else {
		it.Fields[to] = v
	}
	if err := s.refresh(it); err != nil {
		return err
	}
	it.Version++
	if err := s.update(*it); err != nil {
		return err
	}
	return s.record("item.fields_set", *it, map[string]any{"values": map[string]any{to: v}, "copied_from": from},
		ledger.SystemActor)
}

// settle moves an item that waits for approvals once they are complete or one was rejected.
func (s session) settle(it *Item) error {
	a := s.p.Approvals
	if a == nil || it.State != a.In {
		return nil
	}
	st, err := s.status(*it)
	if err != nil || st.Next == "" || st.Next == it.State {
		return err
	}
	action := "approvals_complete"
	if st.Rejected != nil {
		action = "rejected"
	}
	return s.move(it, st.Next, map[string]any{"action": action}, ledger.SystemActor)
}

func (s session) status(it Item) (engine.ApprovalStatus, error) {
	given, lookup, err := s.approvalInputs(it)
	if err != nil {
		return engine.ApprovalStatus{}, err
	}
	return engine.Status(s.p, s.engineItem(it), given, lookup)
}

// approvalInputs loads every decision ever made on the item, and the master data its rules use.
func (s session) approvalInputs(it Item) ([]engine.Approval, engine.MasterLookup, error) {
	rows, _ := s.tx.Query(s.ctx, `
		SELECT slot, person_id::text, decision, item_version, fields_sha256 FROM approvals
		WHERE tenant_id = current_tenant() AND work_item_id::text = $1`, it.ID)
	given, err := pgx.CollectRows(rows, pgx.RowToStructByPos[engine.Approval])
	if err != nil {
		return nil, nil, fmt.Errorf("workitem: decisions: %w", err)
	}
	lookup, err := master.LookupFor(s.ctx, s.tx, s.p, it.Fields)
	if err != nil {
		return nil, nil, err
	}
	return given, lookup, nil
}

func (s session) isMaster(key string) bool {
	f := s.field(key)
	return f != nil && f.Type == "master"
}
