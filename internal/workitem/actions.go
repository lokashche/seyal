// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"cmp"
	"context"
	"errors"
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
	// A schedule's start (D75, R) says which period ("Nov 2026") and why, in plain words ("Monthly
	// schedule: Nov 2026, for Acme Traders"); Extra adds what Seyal went by (today, as_if) to the event.
	Period, Reason string
	Extra          map[string]any
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
	// Numbers start again each year only if the year is part of them; otherwise "T-0001" would come
	// round again next year.
	counter := s.p.ID
	if strings.Contains(s.p.Numbering, "{yyyy}") || strings.Contains(s.p.Numbering, "{yy}") {
		counter = fmt.Sprintf("%s:%d", s.p.ID, year)
	}
	seq, err := TakeNumber(ctx, tx, counter)
	if err != nil {
		return Item{}, err
	}

	it := Item{
		Number:            FormatNumber(s.p.Numbering, year, seq),
		PlaybookVersionID: in.PlaybookVersionID,
		Version:           1,
		Fields:            engine.Fields{},
		StartedVia:        in.Via,
		StartedRef:        in.Ref,
		CreatedBy:         actorPerson(in.By),
	}
	enterStep(&it, s.p.Initial)
	if err := s.apply(&it, in.Fields, in.By.Kind == ledger.System); err != nil {
		return Item{}, err
	}
	left, err := s.unfitOwner(&it, in.Fields)
	if err != nil {
		return Item{}, err
	}
	if err := s.refresh(&it); err != nil {
		return Item{}, err
	}
	if err := s.insert(&it); err != nil {
		return Item{}, err
	}
	change := map[string]any{"via": in.Via, "ref": in.Ref}
	maps.Copy(change, in.Extra)
	for key, v := range map[string]string{"period": in.Period, "reason": in.Reason} {
		if v != "" {
			change[key] = v
		}
	}
	if err := s.record("item.started", it, change, in.By); err != nil {
		return Item{}, err
	}
	if left != "" {
		if err := s.record("item.posted", it, map[string]any{"text": left}, ledger.SystemActor); err != nil {
			return Item{}, err
		}
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
	if err := s.enter(&it, arrival{}); err != nil {
		return Item{}, err
	}
	return it, nil
}

// SetFields changes fields of an item, as a person (or an agent for a person) would. If anything
// really changed, the version goes up, so earlier approvals stop counting, and an approved item
// goes back to waiting for approvals. A change to the field that says who has the item in this step
// (its owner field) is a pass on, with the same checks (D68, choice 14): only whoever has it or an
// admin may make it, and only to someone who may receive it here. The checks look at what the change
// really does, so a field a list entry fills in (a work type's maker) counts too (D72 review fix).
// tx must come from db.WithTenant.
func SetFields(ctx context.Context, tx pgx.Tx, id string, values engine.Fields, by ledger.Actor) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	if err := MayChange(ctx, tx, it, by); err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	changes, err := s.powerChanges(it, values, by)
	if err != nil {
		return Item{}, err
	}
	h, err := s.ownerFieldChange(it, changes, by)
	if err != nil {
		return Item{}, err
	}
	if err := s.powerFieldChange(it, changes, by); err != nil {
		return Item{}, err
	}
	return s.setFields(it, values, by, h)
}

// powerChanges returns values plus every person field that carries power (engine.PowerFields) whose
// value the change really alters, with its new value (nil when cleared). It tries the change on a copy
// of the item, so a value filled in from a list entry (picking a work type fills in its maker) is
// checked like a value a person gives.
func (s session) powerChanges(it Item, values engine.Fields, by ledger.Actor) (engine.Fields, error) {
	try := it
	try.Fields = maps.Clone(it.Fields)
	if try.Fields == nil {
		try.Fields = engine.Fields{}
	}
	if err := s.apply(&try, values, by.Kind == ledger.System); err != nil {
		return nil, err
	}
	changes := engine.Fields{}
	maps.Copy(changes, values)
	for key := range engine.PowerFields(s.p) {
		if !engine.SameValue(try.Fields[key], it.Fields[key]) {
			changes[key] = try.Fields[key]
		}
	}
	return changes, nil
}

// setFields is SetFields after its checks. h is how the item changes hands when the owner field gets
// a new person (nil: Seyal's own change, recorded as the step's owner getting it).
func (s session) setFields(it Item, values engine.Fields, by ledger.Actor, h *handoff) (Item, error) {
	before := it.FieldsHash
	_, ownerKey := engine.Owner(s.p, it.State)
	ownerBefore := it.Fields[ownerKey]
	if err := s.apply(&it, values, by.Kind == ledger.System); err != nil {
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
	if next := engine.StateAfterEdit(s.p, it.State); next != from {
		enterStep(&it, next)
		change["state_from"] = from
	} else if a := s.p.Approvals; a != nil && it.State == a.In {
		// A change while it waits for approvals resets them (a new version), so the clock starts again:
		// whoever must approve again is reminded after the step's wait, not at once (D75, M6).
		enterStep(&it, it.State)
	}
	if err := s.update(it); err != nil {
		return Item{}, err
	}
	if err := s.record("item.fields_set", it, change, by); err != nil {
		return Item{}, err
	}
	if it.State != from {
		if err := s.enter(&it, arrival{}); err != nil {
			return Item{}, err
		}
	} else if kind, name := engine.Owner(s.p, it.State); kind == "field" && it.Fields[name] != nil &&
		(values[name] != nil || !engine.SameValue(it.Fields[name], ownerBefore)) {
		// The step belongs to whoever this field names, so a new name hands the item over: one given
		// here, or one a list entry filled in.
		if h == nil {
			if err := s.handOver(&it, arrival{}); err != nil {
				return Item{}, err
			}
		} else if err := s.assign(&it, it.Fields[name].(string), "", by, *h); err != nil {
			return Item{}, err
		}
	} else if a := s.p.Approvals; a != nil && it.State == a.In {
		// The change may leave no approval needed (each one has a condition): then it moves on.
		if err := s.settle(&it); err != nil {
			return Item{}, err
		}
	}
	return it, nil
}

// Act performs one of the playbook's actions, such as submit or cancel. Who may take it is checked on
// the line of the process the action really takes (its "who", or the default: whoever has the item,
// whoever started it, or an admin). tx must come from db.WithTenant.
func Act(ctx context.Context, tx pgx.Tx, id, action, reason string, by ledger.Actor) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return Item{}, err
	}
	a, err := s.actor(by)
	if err != nil {
		return Item{}, err
	}
	lookup, err := master.LookupFor(ctx, tx, s.p, it.Fields)
	if err != nil {
		return Item{}, err
	}
	eit := s.engineItem(it)
	line, err := engine.Pick(s.p, eit, action, lookup)
	if err != nil {
		return Item{}, err
	}
	change := map[string]any{"action": action}
	if line.By != "seyal" { // Seyal's own moves aren't limited by who; Check refuses them to anyone else
		ok, asAdmin := engine.Allowed(s.p, eit, line.Who, a)
		if !ok {
			return Item{}, s.notAllowed(action, line.Who)
		}
		if asAdmin {
			change["as"] = "admin"
		}
	}
	to, err := engine.Check(s.p, eit, line, reason, by.Kind == ledger.System)
	if err != nil {
		return Item{}, err
	}
	if reason != "" {
		change["reason"] = reason
	}
	in := arrival{action: action, back: line.Back, from: a.ID, note: strings.TrimSpace(reason)}
	if err := s.move(&it, to, change, by, in); err != nil {
		return Item{}, err
	}
	return it, nil
}

// Decide records a person's approval or rejection of one approval slot. Only people (not agents)
// decide, and the engine's rules apply: the right role, one decision per slot and per person for
// each version, and a reason for a rejection. tx must come from db.WithTenant.
func Decide(ctx context.Context, tx pgx.Tx, id, personID, slot, decision, reason string) (Item, error) {
	return DecideSeen(ctx, tx, id, 0, personID, slot, decision, reason)
}

// ErrChanged means the item changed after the person looked at it, so their decision would apply
// to details they haven't seen.
var ErrChanged = errors.New("this item changed after you opened it; please look at it again before deciding")

// DecideSeen is Decide for a person who looked at version seen of the item (0: not known). If the
// item has changed since, nothing is recorded and ErrChanged is returned.
func DecideSeen(ctx context.Context, tx pgx.Tx, id string, seen int, personID, slot, decision, reason string) (Item, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	if seen != 0 && seen != it.Version {
		return Item{}, ErrChanged
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
	// Someone who did an escalated step's work themselves may now check it: the event says so (D75,
	// decision 11).
	worked, err := WorkedEscalated(ctx, tx, it.ID, personID)
	if err != nil {
		return Item{}, err
	}
	if len(worked) > 0 {
		change["worked_escalated"] = worked
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

// StatusOf returns the approval status of it (an item or a ledger snapshot of one) under p, its own
// process version, with every decision ever made on the item: only those for it's version and fields
// count. The phone-note reader uses it outside a web request. tx must come from db.WithTenant.
func StatusOf(ctx context.Context, tx pgx.Tx, p *playbook.Playbook, it Item) (engine.ApprovalStatus, error) {
	return session{ctx: ctx, tx: tx, p: p}.status(it)
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
// happens before the other values, which then win over pre-filled ones. auto means Seyal fills them
// on its own (linked work reporting back), so read-only fields may be filled too.
func (s session) apply(it *Item, values engine.Fields, auto bool) error {
	check := engine.CheckSet
	if auto {
		check = engine.CheckFill
	}
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
		if err := check(s.p, s.engineItem(*it), key, v); err != nil {
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

// unfitOwner checks a new item's first step, when a list entry filled in the person field that owns it
// (D75, R): the person must be able to hold work (mayHold), as when a person sets that field. If they
// can't (no longer active, no login yet), the field is left empty: work Seyal started is then free work
// that anyone the step allows may take, and work a person started stays with them. It returns the
// thread line that says so, or "". A value given directly
// (given) is left as it is: the usual checks already ran on it.
func (s session) unfitOwner(it *Item, given engine.Fields) (string, error) {
	kind, key, _ := strings.Cut(s.p.Steps[it.State].Owner, ":")
	person, _ := it.Fields[key].(string)
	if _, set := given[key]; kind != "field" || person == "" || set {
		return "", nil
	}
	why, err := s.mayHold(person, "")
	if err != nil || why == "" {
		return "", err
	}
	name, err := s.personName(person)
	if err != nil {
		return "", err
	}
	delete(it.Fields, key)
	text := fmt.Sprintf("%s %s, so %s is left empty.", name, why, s.fieldLabel(key))
	if it.CreatedBy == "" {
		text += " Anyone who may work this step can take it." // nobody started it: it is free work
	}
	return text, nil
}

// move puts the item into a new state, records it, and runs what happens on entering it. in says how
// it got there (see arrival). Entering the approval state starts a new approval round: the version
// goes up, so decisions from an earlier round (e.g. a rejection) don't count again.
func (s session) move(it *Item, to string, change map[string]any, by ledger.Actor, in arrival) error {
	from := it.State
	enterStep(it, to)
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
	return s.enter(it, in)
}

// enter hands the item to whoever should have it in its new step, runs the playbook's actions for the
// state, then follows any automatic move (all approvals given, or one rejected). Work sent back (in.back)
// skips the actions: going back is not starting the step afresh (D68, choice 8).
func (s session) enter(it *Item, in arrival) error {
	if err := s.handOver(it, in); err != nil {
		return err
	}
	if in.back {
		return s.settle(it)
	}
	for _, a := range s.p.On[it.State] {
		switch a.Do {
		case "copy":
			if a.IfEmpty && !engine.IsEmpty(it.Fields[a.To]) {
				continue // "only the first time": keep what was copied before
			}
			if err := s.copyField(it, a.From, a.To); err != nil {
				return err
			}
		case "start":
			state := it.State
			if err := s.startChild(it, a); err != nil {
				return err
			}
			if it.State != state {
				return nil // the linked work moved this item on at once; its new step has already run
			}
		case "tell_parent":
			if err := s.tellParent(it, a); err != nil {
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
			if not, err := s.mayHold(id, ""); err != nil || not != "" {
				if err == nil {
					err = s.postStays(it, s.goneWhy(id, not, "is the "+s.fieldLabel(a.To)), false)
				}
				if err != nil {
					return err
				}
				continue
			}
			if err := s.assign(it, id, "", ledger.SystemActor, handoff{How: HowRule}); err != nil {
				return err
			}
		}
	}
	return s.settle(it)
}

// How an item changed hands, in the "how" of its item.assigned event (D68, D70). An event without "how"
// (written before D70) means HowStep.
const (
	HowStep   = "step"   // the new step's owner got it (a rule of the process; recorded as Seyal)
	HowPass   = "pass"   // a person passed it on (recorded as that person)
	HowTake   = "take"   // a person took it from the team (recorded as that person)
	HowReturn = "return" // work sent back went to whoever had that step (recorded as Seyal)
	HowRule   = "rule"   // an "assign" action of the process (recorded as Seyal)
	// HowEscalate: Seyal handed on work that waited too long in its step (D75, M6). Nobody handed it
	// over, so the card says "Seyal escalated it" and not "From ..."; from_holder names who had it.
	HowEscalate = "escalate"
	// HowReassign: an admin moved someone's open work to another person in one go (D75, M3), recorded as
	// the admin. from_assignee is the admin (the card's "From ..."), from_holder who had it, and batch is
	// the same on every move of one press.
	HowReassign = "reassign"
)

// handoff says how and why an item changes hands. How and Note are always asked for; the rest are
// added to the event only when set.
type handoff struct {
	How   string // one of the How… words
	Note  string // why, in the person's words (or the reason work was sent back)
	Via   string // button, panel, chat or voice; "" when a rule did it
	As    string // "admin" when only being an admin allowed it
	After string // the move that sent the work back (HowReturn)
	From  string // who it comes from, when that isn't simply the person who had it (a send-back)
	At    string // the moment of the hand-over (HeldSinceLayout), when it must match another one; "" is now
	// Extra adds keys to the event's change, e.g. why Seyal escalated it and what it counted from.
	Extra map[string]any
}

// assign gives the item to one person, or to a team (a role) when person is "", and records who did
// it (by), how and why. Nothing happens if it is already there. It never changes the version: who
// holds the work isn't one of its details, so approvals already given stay (D68 choice 5).
func (s session) assign(it *Item, person, team string, by ledger.Actor, h handoff) error {
	if it.AssigneeID == person && it.Team == team {
		return nil
	}
	fromAssignee, fromTeam := it.AssigneeID, it.Team
	from := cmp.Or(h.From, fromAssignee)
	if h.How == HowEscalate {
		from = "" // nobody handed it over: Seyal took it from whoever had it (from_holder)
	}
	it.AssigneeID, it.Team = person, team
	it.HeldSince, it.HandoffFrom, it.HandoffNote = cmp.Or(h.At, heldSinceNow()), from, h.Note
	if err := s.update(*it); err != nil {
		return err
	}
	change := map[string]any{"how": cmp.Or(h.How, HowStep), "assignee_id": person}
	maps.Copy(change, h.Extra)
	// from_assignee is who handed it over (the card's "From ..."); from_holder is who had it (D75).
	for key, v := range map[string]string{"team": team, "from_assignee": from, "from_team": fromTeam,
		"from_holder": fromAssignee, "note": h.Note, "via": h.Via, "as": h.As, "after": h.After} {
		if v != "" {
			change[key] = v
		}
	}
	return s.record("item.assigned", *it, change, by)
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
	in := arrival{action: "approvals_complete"}
	if st.Rejected != nil {
		// "From Chitra: <her reason>": a refusal comes back from whoever refused it. With on_reject_back
		// it is a rewind (D68, choice 8); older versions behave as before.
		in.action, in.back = "rejected", a.OnRejectBack
		if in.from, in.note, err = s.rejection(*it, st.Rejected); err != nil {
			return err
		}
	}
	return s.move(it, st.Next, map[string]any{"action": in.action}, ledger.SystemActor, in)
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

// maxDepth limits how many linked items one action may start or move in a chain, so two processes
// that start each other can't loop forever.
const maxDepth = 5

type depthKey struct{}

func deeper(ctx context.Context) (context.Context, error) {
	d, _ := ctx.Value(depthKey{}).(int)
	if d >= maxDepth {
		return ctx, fmt.Errorf("workitem: linked work goes more than %d levels deep; check the processes for a loop", maxDepth)
	}
	return context.WithValue(ctx, depthKey{}, d+1), nil
}

// startChild starts another process as linked work (a child of this item), on the version the
// business uses now, copying the values the action names. Both threads say so.
func (s session) startChild(it *Item, a playbook.Action) error {
	ctx, err := deeper(s.ctx)
	if err != nil {
		return err
	}
	var versionID string
	err = s.tx.QueryRow(ctx, `
		SELECT current_version_id::text FROM tenant_playbooks
		WHERE tenant_id = current_tenant() AND playbook_id = $1 AND active`, a.Process).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("this step starts the process %q, which isn't switched on; add it in Settings → Processes", a.Process)
	}
	if err != nil {
		return err
	}
	fields := engine.Fields{}
	for child, from := range a.Copy {
		if v, ok := it.Fields[from]; ok {
			fields[child] = v
		}
	}
	child, err := Start(ctx, s.tx, NewItem{PlaybookVersionID: versionID, Via: "event", Ref: it.ID, Fields: fields,
		Parent: it.ID, StartedBy: it.ID, By: ledger.SystemActor})
	if err != nil {
		return fmt.Errorf("starting %s: %w", a.Process, err)
	}
	// The child may have reported back at once and changed this item: carry on from what is stored.
	if *it, err = getForUpdate(s.ctx, s.tx, it.ID); err != nil {
		return err
	}
	name := strings.ReplaceAll(a.Process, "-", " ")
	if err := s.record("item.posted", *it, map[string]any{"text": fmt.Sprintf("Started %s %s.", name, child.Number)},
		ledger.SystemActor); err != nil {
		return err
	}
	cs, err := open(ctx, s.tx, child.PlaybookVersionID)
	if err != nil {
		return err
	}
	return cs.record("item.posted", child, map[string]any{"text": fmt.Sprintf("Started from %s.", it.Number)}, ledger.SystemActor)
}

// tellParent reports to the item this one was started from: it copies the values the action names
// into the parent, then takes the parent's action (as Seyal).
func (s session) tellParent(it *Item, a playbook.Action) error {
	ctx, err := deeper(s.ctx)
	if err != nil {
		return err
	}
	var parentID string
	err = s.tx.QueryRow(ctx, `
		SELECT from_item_id::text FROM work_item_links
		WHERE tenant_id = current_tenant() AND to_item_id::text = $1 AND kind = 'parent'`, it.ID).Scan(&parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // started on its own, so there is no one to tell
	}
	if err != nil {
		return err
	}
	// A parent that was closed meanwhile (e.g. cancelled) can't take the report; this item still
	// finishes, and says so.
	parent, err := Get(ctx, s.tx, parentID)
	if err != nil {
		return err
	}
	ps, err := open(ctx, s.tx, parent.PlaybookVersionID)
	if err != nil {
		return err
	}
	if engine.IsClosed(ps.p, parent.State) {
		return s.record("item.posted", *it, map[string]any{"text": fmt.Sprintf(
			"%s is %s, so nothing was passed back to it.", parent.Number, strings.ReplaceAll(parent.State, "_", " "))},
			ledger.SystemActor)
	}
	values := engine.Fields{}
	for parent, from := range a.Copy {
		if ps.field(parent) == nil {
			continue // the parent's (older) version has no such field, so there is nothing to fill
		}
		if v, ok := it.Fields[from]; ok {
			values[parent] = v
		}
	}
	if len(values) > 0 {
		if _, err := SetFields(ctx, s.tx, parentID, values, ledger.SystemActor); err != nil {
			return fmt.Errorf("telling the parent item: %w", err)
		}
	}
	if _, err := Act(ctx, s.tx, parentID, a.Action, "", ledger.SystemActor); err != nil {
		return fmt.Errorf("telling the parent item: %w", err)
	}
	return nil
}
