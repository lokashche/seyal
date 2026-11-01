// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// My Work groups open items by what the person needs to do, and says on each card what happens next.

const (
	needsApproval = iota
	overdue
	yourTurn
	yourTeam
	waitingOnOthers
	otherOpen
)

var groupTitles = []string{"Needs your approval", "Overdue", "Your turn", "For your team", "Waiting on others", "Other open work"}

func (v *view) groups() ([]group, error) {
	items, err := workitem.List(v.ctx, v.tx)
	if err != nil {
		return nil, err
	}
	firstWords, err := v.firstWords(items)
	if err != nil {
		return nil, err
	}
	buckets := make([][]row, len(groupTitles))
	kinds := map[string]bool{}
	type placed struct {
		r      row
		bucket int
		pbID   string
	}
	var all []placed
	for _, it := range items {
		p, err := v.playbook(it)
		if err != nil {
			return nil, err
		}
		if engine.IsClosed(p, it.State) {
			continue
		}
		kinds[p.ID] = true
		bucket, hint, err := v.next(p, it)
		if err != nil {
			return nil, err
		}
		pill, status := v.status(p, it)
		r := row{ID: it.ID, Number: it.Number, Title: it.Title, Pill: pill, Status: status, Hint: hint}
		if r.Title == "" {
			r.Title, r.Quoted = firstWords[it.ID], firstWords[it.ID] != ""
		}
		all = append(all, placed{r, bucket, p.ID})
	}
	for _, pl := range all {
		if len(kinds) > 1 {
			pl.r.Kind = human(pl.pbID)
		}
		buckets[pl.bucket] = append(buckets[pl.bucket], pl.r)
	}
	var out []group
	for i, rows := range buckets {
		if len(rows) > 0 {
			out = append(out, group{Title: groupTitles[i], Rows: rows})
		}
	}
	return out, nil
}

// next says which group an item belongs to for the person, and what happens next, in a few words.
func (v *view) next(p *playbook.Playbook, it workitem.Item) (bucket int, hint string, err error) {
	s := v.c.session
	involved := it.CreatedBy == s.PersonID || it.AssigneeID == s.PersonID
	elsewhere := func(h string) (int, string, error) {
		if involved {
			return waitingOnOthers, h, nil
		}
		return otherOpen, h, nil
	}

	if a := p.Approvals; a != nil && it.State == a.In {
		st, err := workitem.Approvals(v.ctx, v.tx, it.ID)
		if err != nil {
			return 0, "", err
		}
		if canDecide(st, s) != nil {
			return needsApproval, "Your approval is needed", nil
		}
		names := make([]string, len(st.Waiting))
		for i, slot := range st.Waiting {
			names[i] = human(slot)
		}
		return elsewhere("Waiting for approval: " + strings.Join(names, ", "))
	}
	if waitsForLinked(p, it.State) {
		h, err := v.waitingFor(it)
		if err != nil {
			return 0, "", err
		}
		return elsewhere(h)
	}

	// Who should act now: the assignee, the team, or (in processes without owners) whoever started it.
	mine := it.AssigneeID == s.PersonID || (it.AssigneeID == "" && it.Team == "" && it.CreatedBy == s.PersonID)
	team := it.AssigneeID == "" && it.Team != "" && slices.Contains(s.Roles, it.Team)
	if !mine && !team {
		return elsewhere("With " + v.holder(it))
	}
	hint = "Your turn"
	if toAsk := engine.ToAsk(p, it.State, it.Fields); len(toAsk) > 0 {
		for _, f := range p.Fields {
			if f.Key == toAsk[0] {
				hint = "Seyal is asking: " + cmp.Or(f.Ask, ask.Label(f))
			}
		}
	} else if len(engine.Missing(p, it.State, it.Fields)) == 0 {
		for _, t := range p.Transitions {
			if t.When == "required_filled" && t.By == "" && slices.Contains(t.From, it.State) {
				hint = fmt.Sprintf("Ready: say %q", strings.ReplaceAll(t.Action, "_", " "))
				break
			}
		}
	}
	switch {
	case it.DueOn != "" && it.DueOn < v.today:
		return overdue, hint, nil
	case mine:
		return yourTurn, hint, nil
	}
	return yourTeam, hint, nil
}

// waitsForLinked reports whether the item waits in a state that only Seyal moves on from, e.g. until
// linked work reports back.
func waitsForLinked(p *playbook.Playbook, state string) bool {
	return slices.ContainsFunc(p.Transitions, func(t playbook.Transition) bool {
		return t.By == "seyal" && slices.Contains(t.From, state)
	})
}

// waitingFor names the open linked work an item waits for, and who has it.
func (v *view) waitingFor(it workitem.Item) (string, error) {
	rows, _ := v.tx.Query(v.ctx, `
		SELECT to_item_id::text FROM work_item_links
		WHERE tenant_id = current_tenant() AND from_item_id::text = $1 AND kind = 'parent' ORDER BY created_at`, it.ID)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		child, err := workitem.Get(v.ctx, v.tx, id)
		if err != nil {
			return "", err
		}
		cp, err := v.playbook(child)
		if err != nil {
			return "", err
		}
		if !engine.IsClosed(cp, child.State) {
			return fmt.Sprintf("Waiting for %s %s (%s)", human(cp.ID), child.Number, v.holder(child)), nil
		}
	}
	return "Waiting for linked work", nil
}

// holder names who has an item: a person, a team, or nobody yet.
func (v *view) holder(it workitem.Item) string {
	switch {
	case it.AssigneeID != "":
		return v.show(playbook.Field{Type: "person"}, it.AssigneeID)
	case it.Team != "":
		var name string
		_ = v.tx.QueryRow(v.ctx, `SELECT name FROM roles WHERE tenant_id = current_tenant() AND key = $1`, it.Team).Scan(&name)
		return cmp.Or(name, it.Team) + " team"
	}
	return "nobody yet"
}

// firstWords gives untitled items something to recognise them by: the start of the message that
// started them.
func (v *view) firstWords(items []workitem.Item) (map[string]string, error) {
	var refs []string
	byRef := map[string]string{}
	for _, it := range items {
		if it.Title == "" && it.StartedRef != "" {
			refs = append(refs, it.StartedRef)
			byRef[it.StartedRef] = it.ID
		}
	}
	out := map[string]string{}
	if len(refs) == 0 {
		return out, nil
	}
	rows, _ := v.tx.Query(v.ctx, `
		SELECT id::text, text FROM messages WHERE tenant_id = current_tenant() AND id::text = ANY($1)`, refs)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Text string }])
	if err != nil {
		return nil, err
	}
	for _, m := range list {
		if text := strings.Join(strings.Fields(m.Text), " "); text != "" {
			if r := []rune(text); len(r) > 60 {
				text = string(r[:60]) + "…"
			}
			out[byRef[m.ID]] = text
		}
	}
	return out, nil
}

// links lists the item this one is part of, then the items it started.
func (v *view) links(id string) ([]linkView, error) {
	rows, _ := v.tx.Query(v.ctx, `
		SELECT CASE WHEN to_item_id::text = $1 THEN from_item_id::text ELSE to_item_id::text END,
			CASE WHEN to_item_id::text = $1 THEN 'Part of' ELSE 'Started' END
		FROM work_item_links
		WHERE tenant_id = current_tenant() AND kind = 'parent' AND (from_item_id::text = $1 OR to_item_id::text = $1)
		ORDER BY 2 DESC, created_at`, id)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Relation string }])
	if err != nil {
		return nil, err
	}
	var out []linkView
	for _, l := range list {
		it, err := workitem.Get(v.ctx, v.tx, l.ID)
		if err != nil {
			return nil, err
		}
		p, err := v.playbook(it)
		if err != nil {
			return nil, err
		}
		_, status := v.status(p, it)
		out = append(out, linkView{ID: it.ID, Number: it.Number, Title: it.Title, Kind: sentence(p.ID), Status: status,
			Relation: l.Relation})
	}
	return out, nil
}
