// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

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

// folders are My Work's groups, in the order of the buckets above; "all" shows every open item.
var folders = []struct{ key, title string }{
	{"approval", "Needs my approval"}, {"overdue", "Overdue"}, {"turn", "My turn"}, {"team", "For my team"},
	{"waiting", "Waiting on others"}, {"all", "All open work"},
}

// myWork is the folder column and the list. The column has two parts that each add up: the person's
// own folders, and all open work (in total and per process). The list shows the chosen folder (?f=)
// or process (?p=, which always means all open work of that process), in the chosen order
// (?s=recent or due).
type myWork struct {
	Folders   []folder // the person's own
	All       folder
	Processes []folder
	Rows      []row
	Title     string
	Sort      string
	Query     string // "?f=…&p=…&s=…", kept on links so the list stays as chosen
}

type folder struct {
	Key, Title, URL string
	Count           int
	Current         bool
}

func (v *view) myWork() (*myWork, error) {
	items, err := workitem.List(v.ctx, v.tx)
	if err != nil {
		return nil, err
	}
	extra, err := v.rowExtras(items)
	if err != nil {
		return nil, err
	}
	type placed struct {
		r      row
		bucket int
		pbID   string
		due    string
	}
	var all []placed
	byProcess := map[string]int{}
	counts := make([]int, len(folders))
	kinds := map[string]bool{}
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
		x := extra[it.ID]
		r := row{ID: it.ID, Number: it.Number, Title: it.Title, Pill: pill, Status: status, Hint: hint,
			Preview: x.preview, Ago: x.ago, Owner: initials(v.holder(it)), OwnerName: v.holder(it),
			Mine: bucket <= yourTeam}
		if untitled(r.Title) {
			r.Title, r.Quoted = x.firstWords, x.firstWords != ""
		}
		all = append(all, placed{r, bucket, p.ID, it.DueOn})
		byProcess[p.ID]++
		if bucket < otherOpen {
			counts[bucket]++
		}
		counts[len(folders)-1]++ // all open
	}

	q := v.q
	w := &myWork{Sort: q.Get("s")}
	if w.Sort != "due" {
		w.Sort = "recent"
	}
	chosen := q.Get("f")
	if !slices.ContainsFunc(folders, func(f struct{ key, title string }) bool { return f.key == chosen }) {
		chosen = "all"
		for _, k := range []int{needsApproval, overdue, yourTurn, yourTeam} {
			if counts[k] > 0 {
				chosen = folders[k].key
				break
			}
		}
	}
	proc := q.Get("p")
	if byProcess[proc] == 0 {
		proc = ""
	}
	if proc != "" {
		chosen = "all" // a process is a part of all open work, never of a personal folder
	}
	link := func(f, p string) string {
		vals := url.Values{}
		vals.Set("f", f)
		if p != "" {
			vals.Set("p", p)
		}
		if w.Sort != "recent" {
			vals.Set("s", w.Sort)
		}
		return "?" + vals.Encode()
	}
	w.Query = link(chosen, proc)
	for i, f := range folders {
		fl := folder{Key: f.key, Title: f.title, Count: counts[i], Current: f.key == chosen && proc == "", URL: link(f.key, "")}
		if f.key == "all" {
			w.All = fl
		} else {
			w.Folders = append(w.Folders, fl)
		}
		if f.key == chosen {
			w.Title = f.title
		}
	}
	if len(kinds) > 1 {
		for _, id := range slices.Sorted(maps.Keys(byProcess)) {
			w.Processes = append(w.Processes, folder{Key: id, Title: sentence(id), Count: byProcess[id],
				Current: id == proc, URL: link("all", id)})
		}
	}
	if proc != "" {
		w.Title = sentence(proc)
	}

	for _, pl := range all {
		in := chosen == "all" || folders[pl.bucket].key == chosen
		if pl.bucket == otherOpen {
			in = chosen == "all"
		}
		if !in || (proc != "" && pl.pbID != proc) {
			continue
		}
		if len(kinds) > 1 && proc == "" {
			pl.r.Kind = human(pl.pbID)
		}
		w.Rows = append(w.Rows, pl.r)
	}
	if w.Sort == "due" {
		dues := map[string]string{}
		for _, pl := range all {
			dues[pl.r.ID] = pl.due
		}
		slices.SortStableFunc(w.Rows, func(a, b row) int {
			da, db := dues[a.ID], dues[b.ID]
			switch {
			case da == db:
				return 0
			case da == "":
				return 1
			case db == "":
				return -1
			}
			return strings.Compare(da, db)
		})
	}
	return w, nil
}

// untitled reports whether a title says nothing yet: empty, or only the punctuation of a title
// pattern whose fields are still empty ("—").
func untitled(title string) bool {
	return !strings.ContainsFunc(title, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

// initials shortens a name for the small badge on a card: "Priya Raman" → "PR".
func initials(name string) string {
	var out []rune
	for _, w := range strings.Fields(name) {
		if r := []rune(w); len(out) < 2 && len(r) > 0 {
			out = append(out, r[0])
		}
	}
	return strings.ToUpper(string(out))
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
		return cmp.Or(v.roles[it.Team], it.Team) + " team"
	}
	return "nobody yet"
}

// rowExtra is what a card shows besides the item itself.
type rowExtra struct {
	firstWords string // the start of the message that started it, for untitled items
	preview    string // the start of the latest message in its thread
	ago        string // how long since it last changed, e.g. "3h"
}

// rowExtras finds, for every item, the start of its first and latest messages and when it last
// changed, in three queries.
func (v *view) rowExtras(items []workitem.Item) (map[string]rowExtra, error) {
	out := map[string]rowExtra{}
	short := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if r := []rune(s); len(r) > 70 {
			s = string(r[:70]) + "…"
		}
		return s
	}
	rows, _ := v.tx.Query(v.ctx, `
		SELECT DISTINCT ON (work_item_id) work_item_id::text, text FROM messages
		WHERE tenant_id = current_tenant() AND work_item_id IS NOT NULL AND text <> ''
		ORDER BY work_item_id, sent_at DESC`)
	latest, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Text string }])
	if err != nil {
		return nil, err
	}
	for _, m := range latest {
		x := out[m.ID]
		x.preview = short(m.Text)
		out[m.ID] = x
	}
	rows, _ = v.tx.Query(v.ctx, `
		SELECT w.id::text, m.text FROM work_items w JOIN messages m ON m.tenant_id = w.tenant_id AND m.id::text = w.started_ref
		WHERE w.tenant_id = current_tenant()`)
	first, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Text string }])
	if err != nil {
		return nil, err
	}
	for _, m := range first {
		x := out[m.ID]
		x.firstWords = short(m.Text)
		out[m.ID] = x
	}
	rows, _ = v.tx.Query(v.ctx, `SELECT id::text, updated_at FROM work_items WHERE tenant_id = current_tenant()`)
	changed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID string
		At time.Time
	}])
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, c := range changed {
		x := out[c.ID]
		x.ago = ago(now.Sub(c.At), c.At.In(v.c.tenant.Zone))
		out[c.ID] = x
	}
	return out, nil
}

// ago says how long ago something happened, briefly: "now", "5m", "3h", "2d", or the date.
func ago(d time.Duration, at time.Time) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return at.Format("2 Jan")
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
