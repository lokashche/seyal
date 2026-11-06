// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Deadlines that act (D75, D77; Phase 2, M6). The chaser (internal/chase) works out what is due; these
// functions do it, as Seyal (system), each in the caller's transaction:
//
//   - Remind writes a fixed sentence in the thread for whoever must act now, and marks the step stay
//     (reminded_at, reminders), so it is never written twice.
//   - Escalate hands the item on to the step's escalate_to (the Owner team, or the person in a field),
//     and marks the stay escalated. It never changes the version, so approvals already given stay.
//
// The sentences are fixed: they name the step, the days, the holder and the labels of missing fields,
// never field values or notes.

// Chase is what the chaser knows when it reminds or escalates an item.
type Chase struct {
	// StepSince and Reminders are the step stay as the chaser saw it; if the item is in another stay
	// now, or another reminder came first, nothing happens.
	StepSince string
	Reminders int
	Days      int            // working days the stay has waited, by the chaser's today
	Now       time.Time      // the real moment: what is stored (reminded_at, escalated_at)
	Zone      *time.Location // the business's time zone, for the days in the sentences
	// Inputs say why, in every event: today, days_off, the holidays in the count, step_since, the
	// step's remind_after and escalate_after (and as_if for a pretend day).
	Inputs map[string]any
}

// Timed returns the open items whose step has timing (remind_after) in their own process version and
// that weren't escalated yet in their step stay, oldest stay first, with the process versions by id.
// Items on versions without timing (for example older sample versions) are never in it. tx must come
// from db.WithTenant.
func Timed(ctx context.Context, tx pgx.Tx) ([]Item, map[string]*playbook.Playbook, error) {
	rows, _ := tx.Query(ctx, `
		SELECT DISTINCT v.id::text, v.source FROM work_items w
		JOIN playbook_versions v ON v.tenant_id = w.tenant_id AND v.id = w.playbook_version_id
		WHERE w.tenant_id = current_tenant() AND w.escalated_at IS NULL`)
	versions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Source string }])
	if err != nil {
		return nil, nil, fmt.Errorf("workitem: timed: %w", err)
	}
	books := map[string]*playbook.Playbook{}
	var steps []string // "version id:state"
	for _, v := range versions {
		p, err := playbook.Parse([]byte(v.Source))
		if err != nil {
			return nil, nil, fmt.Errorf("workitem: timed: %w", err)
		}
		for state, st := range p.Steps {
			if remind, _ := st.Timing(); remind > 0 && !engine.IsClosed(p, state) {
				steps = append(steps, v.ID+":"+state)
				books[v.ID] = p
			}
		}
	}
	if len(steps) == 0 {
		return nil, books, nil
	}
	rows, err = tx.Query(ctx, selectItems+`
		AND escalated_at IS NULL AND (playbook_version_id::text || ':' || state) = ANY($1::text[])
		ORDER BY step_since NULLS FIRST, number`, steps)
	if err != nil {
		return nil, nil, fmt.Errorf("workitem: timed: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, it)
	}
	return items, books, rows.Err()
}

// Remind writes Seyal's reminder in the item's thread and marks the step stay (one more reminder). It
// returns what it did in a few words for the log (never a name), or "" when it was no longer due. tx
// must come from db.WithTenant.
func Remind(ctx context.Context, tx pgx.Tx, id string, c Chase) (string, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if it.StepSince != c.StepSince || it.Reminders != c.Reminders || it.Reminders >= 2 || it.EscalatedAt != "" {
		return "", nil // it moved, or someone (another copy of the app) did this first
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return "", err
	}
	text, to, did, err := s.reminder(it, c)
	if err != nil || text == "" {
		return "", err // "": nobody to remind (no approval is missing)
	}
	reason := fmt.Sprintf("it waited %s in %s (remind after %s)", workingDays(c.Days), stepName(it.State),
		s.p.Steps[it.State].RemindAfter)
	if it.Reminders > 0 {
		reason = fmt.Sprintf("it waited %s in %s and whoever has it now wasn't reminded yet, so they are told before it is handed on",
			workingDays(c.Days), stepName(it.State))
	}
	it.RemindedAt, it.Reminders = c.stamp(it), it.Reminders+1
	if err := s.update(it); err != nil {
		return "", err
	}
	change := map[string]any{"text": text, "chase": "remind", "to": to, "days": c.Days, "reason": reason}
	maps.Copy(change, c.Inputs)
	if err := s.record("item.posted", it, change, ledger.SystemActor); err != nil {
		return "", err
	}
	return did, nil
}

// NobodyToRemind reports whether the item waits in its process's approvals step with no approval
// missing. That happens when master data changes so a rule no longer asks for an approval: nothing
// moves the item until someone acts on it. A reminder would have nobody to name, so the chaser leaves
// it, instead of locking the business's ledger for it every round. tx must come from db.WithTenant.
func NobodyToRemind(ctx context.Context, tx pgx.Tx, it Item) (bool, error) {
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return false, err
	}
	if a := s.p.Approvals; a == nil || it.State != a.In {
		return false, nil
	}
	st, err := s.status(it)
	return err == nil && len(st.Waiting) == 0, err
}

// reminder is the reminder's sentence (see the design's table), who it is for ("person:<id>",
// "team:<role>", "nobody" or "approvals:<slots>"), and a few words for the log.
func (s session) reminder(it Item, c Chase) (text, to, did string, err error) {
	roleNames, err := s.roleNames()
	if err != nil {
		return "", "", "", err
	}
	waited, since := workingDays(c.Days), dayOf(it.StepSince, c.Zone)
	step := stepName(it.State)
	if a := s.p.Approvals; a != nil && it.State == a.In {
		st, err := s.status(it)
		if err != nil {
			return "", "", "", err
		}
		var roles []string
		for _, slot := range a.Slots {
			if slices.Contains(st.Waiting, slot.Slot) && !slices.Contains(roles, slot.Role) {
				roles = append(roles, slot.Role)
			}
		}
		if len(roles) == 0 {
			return "", "", "", nil
		}
		names := make([]string, len(roles))
		for i, r := range roles {
			names[i] = cmp.Or(roleNames[r], r)
		}
		return fmt.Sprintf("Reminder: waiting %s (since %s) for approval from %s.", waited, since, andList(names)),
			"approvals:" + strings.Join(st.Waiting, ","), "reminded approvers", nil
	}
	still := " " + s.stillNeeded(it)
	members, err := Members(s.ctx, s.tx)
	if err != nil {
		return "", "", "", err
	}
	person := it.AssigneeID
	switch {
	case person == "" && it.Team != "":
		return fmt.Sprintf("Reminder for %s: this has waited %s (since %s) for someone to take it.%s",
			cmp.Or(roleNames[it.Team], it.Team), waited, since, still), "team:" + it.Team, "reminded team", nil
	case person == "" && Free(it, members):
		return fmt.Sprintf("Reminder: nobody has taken this; it has been in %s for %s (since %s). %s can take it.",
			step, waited, since, sentence(s.describePassTo(it, roleNames))), "nobody", "reminded (nobody has it)", nil
	case person == "":
		person = it.CreatedBy // with whoever started it, while nobody else has it
	}
	name, err := s.personName(person)
	if err != nil {
		return "", "", "", err
	}
	with := ""
	if it.AssigneeID != "" && it.HeldSince != "" && dateOf(it.HeldSince, c.Zone) > dateOf(it.StepSince, c.Zone) {
		with = "with you since " + dayOf(it.HeldSince, c.Zone) + "; "
	}
	return fmt.Sprintf("Reminder for %s: %sthis has been in %s for %s (since %s).%s", name, with, step, waited, since, still),
		"person:" + person, "reminded holder", nil
}

// stillNeeded ends a reminder: the labels of the step's fields still empty, or that nothing is.
func (s session) stillNeeded(it Item) string {
	missing := engine.Missing(s.p, it.State, it.Fields)
	if len(missing) == 0 {
		return "Everything asked here is filled in; it waits for the next move."
	}
	labels := make([]string, len(missing))
	for i, key := range missing {
		labels[i] = s.fieldLabel(key)
	}
	return "Still needed: " + strings.Join(labels, ", ") + "."
}

// Escalate hands the item to its step's escalate_to (D75, M6): the role's team, or the person a field
// names. Nobody who can receive it: nothing moves, and the thread says so. Already with them: nothing
// moves, and the thread says it is still waiting. Either way the stay is marked escalated, so this
// happens once. It returns a few words for the log (never a name), or "" when it was no longer due. tx
// must come from db.WithTenant.
func Escalate(ctx context.Context, tx pgx.Tx, id string, c Chase) (string, error) {
	it, err := getForUpdate(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if it.StepSince != c.StepSince || it.Reminders != c.Reminders || it.EscalatedAt != "" {
		return "", nil
	}
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return "", err
	}
	target := s.p.Steps[it.State].EscalateTo
	if target == "" {
		return "", fmt.Errorf("its step %s has no escalate_to", it.State)
	}
	roleNames, err := s.roleNames()
	if err != nil {
		return "", err
	}
	members, err := Members(ctx, tx)
	if err != nil {
		return "", err
	}
	waited := fmt.Sprintf("%s in %s (since %s)", workingDays(c.Days), stepName(it.State), dayOf(it.StepSince, c.Zone))
	now := c.stamp(it)
	change := map[string]any{"chase": "escalate", "days": c.Days, "escalate_to": target,
		"reason": fmt.Sprintf("it waited %s in %s, and the full gap had passed since the last reminder (escalate after %s)",
			workingDays(c.Days), stepName(it.State), s.p.Steps[it.State].EscalateAfter)}
	maps.Copy(change, c.Inputs)
	kind, name, _ := strings.Cut(target, ":")
	targetName := TeamName(name, roleNames)
	if kind == "field" {
		targetName = "the " + s.fieldLabel(name)
		if id := fieldPerson(it, name); id != "" {
			if targetName, err = s.personName(id); err != nil {
				return "", err
			}
		}
	}
	post := func(text, did string) (string, error) { // nothing moves
		it.EscalatedAt = c.unmoved(it)
		if err := s.update(it); err != nil {
			return "", err
		}
		change["text"] = text
		if err := s.record("item.posted", it, change, ledger.SystemActor); err != nil {
			return "", err
		}
		return did, nil
	}
	if WithEscalationTarget(s.p, it, members) {
		return post(fmt.Sprintf("Still waiting after %s. It is already with %s.", waited, targetName), "already with the escalation's receiver")
	}

	var person, team, why string
	switch kind {
	case "role":
		team = name
		if !slices.ContainsFunc(members, func(m Member) bool { return slices.Contains(m.Roles, name) }) {
			why = "nobody in " + cmp.Or(roleNames[name], name) + " can take work"
		}
	case "field":
		person = fieldPerson(it, name)
		if person == "" {
			why = "nobody is named as " + s.fieldLabel(name)
		} else if not, err := s.mayHold(person, ""); err != nil {
			return "", err
		} else if not != "" {
			why = s.goneWhy(person, not, "is the "+s.fieldLabel(name))
		}
	}
	if why != "" {
		stays := "It stays with " + s.holderName(it, roleNames) + "."
		if it.AssigneeID == "" && it.Team == "" {
			stays = "It stays where it is."
		}
		change["reason"] = "it waited too long, but " + why
		return post(fmt.Sprintf("Couldn't escalate: %s. %s", why, stays), "couldn't escalate")
	}

	note := "waited " + waited
	if it.AssigneeID != "" || it.Team != "" {
		note += ", with " + s.holderName(it, roleNames) + " since " + dayOf(cmp.Or(it.HeldSince, it.StepSince), c.Zone)
	}
	it.EscalatedAt = now
	if err := s.assign(&it, person, team, ledger.SystemActor, handoff{How: HowEscalate, Note: note + ".", At: now, Extra: change}); err != nil {
		return "", err
	}
	return "escalated", nil
}

// WithEscalationTarget reports whether the item is already with whoever its step escalates to: that
// role's team, a person with that role, or the person in that field. members are the business's people
// who may hold work.
func WithEscalationTarget(p *playbook.Playbook, it Item, members []Member) bool {
	kind, name, _ := strings.Cut(p.Steps[it.State].EscalateTo, ":")
	switch {
	case kind == "role" && it.AssigneeID == "":
		return it.Team == name
	case kind == "role":
		return slices.ContainsFunc(members, func(m Member) bool { return m.ID == it.AssigneeID && slices.Contains(m.Roles, name) })
	case kind == "field":
		return it.AssigneeID != "" && it.AssigneeID == fieldPerson(it, name)
	}
	return false
}

// CouldNotEscalate reports whether Seyal wanted to escalate the item in this step stay but nobody could
// receive it, and nobody has moved it since: admins see it, so they can pass it on (D75, M6).
func CouldNotEscalate(p *playbook.Playbook, it Item, members []Member) bool {
	return it.EscalatedAt != "" && !it.EscalatedTo() && it.HeldSince <= it.EscalatedAt &&
		!WithEscalationTarget(p, it, members)
}

// stepName is how a reminder names a step: "With ops".
func stepName(state string) string {
	return sentence(strings.ReplaceAll(state, "_", " "))
}

// sentence starts text with a capital letter.
func sentence(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// workingDays says a number of working days: "1 working day", "2 working days".
func workingDays(n int) string {
	if n == 1 {
		return "1 working day"
	}
	return fmt.Sprintf("%d working days", n)
}

// andList joins names as people say them: "Owner", "Operations and Owner", "Sales, Operations and Owner".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// stamp is the moment a reminder or an escalation is stored with: the chaser's Now, but never before
// the item's latest hand-over. The chaser reads Now once per business, before its round; someone may
// pass the item on after that and before the chaser locks it. Stored before that hand-over, the
// reminder would look older than the new holder's turn (so they would be reminded again), and a failed
// escalation would look undone.
func (c Chase) stamp(it Item) string {
	return max(stampOf(c.Now), it.HeldSince) // both in HeldSinceLayout, so text order is time order
}

// unmoved is the moment an escalation that moves nothing is stored with: stamp, but always after the
// latest hand-over. EscalatedTo reads escalated_at equal to held_since as "Seyal handed it on", so an
// equal stamp (someone passed the item on after the chaser read Now) would make a failed escalation look
// done: no "Couldn't escalate" card for the admins, and the wrong people counted as holders.
func (c Chase) unmoved(it Item) string {
	at := c.stamp(it)
	if t, err := time.Parse(HeldSinceLayout, at); err == nil && at == it.HeldSince {
		at = stampOf(t.Add(time.Microsecond))
	}
	return at
}

// stampOf writes a moment as the snapshot keeps it (HeldSinceLayout).
func stampOf(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(HeldSinceLayout)
}

// dayOf names the day of a stored moment in zone, as reminders do: "Mon 30 Nov".
func dayOf(stamp string, zone *time.Location) string {
	t, err := time.Parse(HeldSinceLayout, stamp)
	if err != nil {
		return "an earlier day"
	}
	return t.In(cmp.Or(zone, time.UTC)).Format("Mon 2 Jan")
}

// dateOf is the date of a stored moment in zone, written 2026-11-30, so two dates compare as text.
func dateOf(stamp string, zone *time.Location) string {
	t, err := time.Parse(HeldSinceLayout, stamp)
	if err != nil {
		return ""
	}
	return t.In(cmp.Or(zone, time.UTC)).Format(time.DateOnly)
}
