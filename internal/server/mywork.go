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
	"seyal/internal/start"
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

// lists are the briefing's two lists that aren't folders (D75, B): the person's cards due today, from
// their four own folders, and their New cards. They open only from the briefing line.
var lists = map[string]string{"today": "Due today", "new": "New for you"}

// withProcess are the lists that may be narrowed to one process: the five the briefing line links to
// (For my team for the admins' parts), so a line that counts one process opens just what it counted.
// Only a link that names the list does so: ?p= alone is all the process's open work. The folder column
// never combines a process with a folder (D51): a process there is all its open work.
var withProcess = map[string]bool{"approval": true, "overdue": true, "today": true, "new": true, "team": true}

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
	Process   string // the chosen process's id, when the list shows one process
	Sort      string
	Query     string // "?f=…&p=…&s=…", kept on links so the list stays as chosen
	Brief     brief  // the line at the top of the list
}

// brief is the daily briefing (D75, B): one line about the person's own work today, worked out on
// every page from the same cards as the folders, so the line and the folders always agree. Nothing is
// stored and nothing goes to the ledger.
type brief struct {
	Day   string      // "Fri 4 Dec", in the business's time zone
	Parts []briefPart // the counts that aren't zero; none means "Nothing waiting on you today."
}

// briefPart is one count of the line, for example "2 due today", and the list it opens.
type briefPart struct {
	Text, URL string // URL is relative to the business's address: "?f=today" or "settings/processes/<id>"
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
	// Which items the person may see is decided in one place (D75, decision 12): today everyone active
	// sees every item, and someone no longer active sees an empty My Work, as in Ask Seyal.
	if items, err = workitem.Visible(v.ctx, v.tx, v.c.session.PersonID, items); err != nil {
		return nil, err
	}
	seen, err := workitem.SeenBy(v.ctx, v.tx, v.c.session.PersonID)
	if err != nil {
		return nil, err
	}
	type placed struct {
		r      row
		bucket int
		pbID   string
		due    string
		fresh  bool // New, counting the item open on this page too (so the "new" list keeps its card)
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
		bucket, hint, take, err := v.next(p, it)
		if err != nil {
			return nil, err
		}
		pill, status := v.status(p, it)
		x := extra[it.ID]
		r := row{ID: it.ID, Number: it.Number, Title: it.Title, Pill: pill, Status: status, Hint: hint,
			Preview: x.preview, Ago: x.ago, OwnerName: v.holder(it), Mine: bucket <= yourTeam}
		if take {
			r.Take = it.HolderKey()
		}
		// New: a card for the person (one of their own folders) that came to them, or their team, since
		// they last saw it (D75, M7). A reminder doesn't change hands, so it never makes a card New.
		fresh := r.Mine && workitem.IsNew(it, v.c.session.PersonID, seen[it.ID])
		r.New = fresh && it.ID != v.opened
		if it.AssigneeID != "" || it.Team != "" {
			r.Owner = initials(r.OwnerName)
		}
		if untitled(r.Title) {
			r.Title, r.Quoted = x.firstWords, x.firstWords != ""
		}
		all = append(all, placed{r, bucket, p.ID, it.DueOn, fresh})
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
	if lists[chosen] == "" && !slices.ContainsFunc(folders, func(f struct{ key, title string }) bool { return f.key == chosen }) {
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
	if proc != "" && !(withProcess[chosen] && q.Get("f") == chosen) {
		// In the folder column a process is a part of all open work, never of a personal folder; only a
		// briefing link (which names its list) narrows a list to one process. Without ?f=, the folder
		// chosen above is the person's first one with cards, which the link didn't ask for.
		chosen = "all"
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
	w.Title = cmp.Or(lists[chosen], w.Title)
	if len(kinds) > 1 {
		for _, id := range slices.Sorted(maps.Keys(byProcess)) {
			w.Processes = append(w.Processes, folder{Key: id, Title: sentence(id), Count: byProcess[id],
				Current: id == proc && chosen == "all", URL: link("all", id)})
		}
	}
	if proc != "" {
		if chosen != "all" {
			w.Title += " · " + sentence(proc) // a list the briefing line opened, for one process
		} else {
			w.Title = sentence(proc)
		}
		w.Process = proc
	}

	// The briefing counts the cards the folders hold, only the open process's when one is open.
	var late, today, fresh, approve, gone, lost, nobody, stuck int
	for _, pl := range all {
		if proc != "" && pl.pbID != proc {
			continue
		}
		switch pl.bucket {
		case needsApproval:
			approve++
		case overdue:
			late++
		}
		if pl.bucket <= yourTeam && pl.due == v.today {
			today++
		}
		if pl.r.New {
			fresh++
		}
		switch v.flags[pl.r.ID] {
		case flagGone:
			gone++
		case flagRole:
			lost++
		case flagNobody:
			nobody++
		case flagEscalate:
			stuck++
		}
	}
	if day, err := time.Parse(time.DateOnly, v.today); err == nil {
		w.Brief.Day = day.Format("Mon 2 Jan")
	}
	add := func(n int, text, to string) {
		if n > 0 {
			w.Brief.Parts = append(w.Brief.Parts, briefPart{Text: fmt.Sprintf("%d %s", n, text), URL: to})
		}
	}
	add(late, "overdue", link("overdue", proc))
	add(today, "due today", link("today", proc))
	add(fresh, "new for you", link("new", proc))
	add(approve, "waiting on your approval", link("approval", proc))
	if v.c.admin() {
		add(gone, "with someone no longer active", link("team", proc))
		add(lost, "with someone no longer in the step's role", link("team", proc))
		add(nobody, "that nobody can take", link("team", proc))
		add(stuck, "couldn't escalate", link("team", proc))
		held, err := v.heldSchedules(proc)
		if err != nil {
			return nil, err
		}
		for _, id := range held {
			// One part per process, so each opens the process whose warning it counted.
			add(1, "schedule held back in "+sentence(id), "settings/processes/"+url.PathEscape(id))
		}
	}

	for _, pl := range all {
		in := chosen == "all" || folders[pl.bucket].key == chosen
		switch chosen {
		case "today":
			in = pl.bucket <= yourTeam && pl.due == v.today
		case "new":
			in = pl.fresh
		}
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

// Why an admin must step in on a card (D75, M3 and M6), as next finds it, for the briefing's admin
// counts.
const (
	flagGone     = "gone"     // its holder is no longer active
	flagRole     = "role"     // its holder is no longer in the step's role
	flagNobody   = "nobody"   // nobody can take it (D68): its team has nobody active, or nobody may take it
	flagEscalate = "escalate" // Seyal couldn't escalate it: nobody could receive it
)

// heldSchedules lists the processes, switched on, whose schedule is held back (too many list entries
// match) or stopped (its list lacks what it needs), as Settings → Processes warns (D75, R); only the
// open process, when one is open.
func (v *view) heldSchedules(proc string) ([]string, error) {
	plans, err := start.Find(v.ctx, v.tx, time.Now().In(v.c.tenant.Zone))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range plans {
		if (proc == "" || p.PlaybookID == proc) && (p.Problem != "" || p.HeldBack()) {
			ids = append(ids, p.PlaybookID)
		}
	}
	return ids, nil
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
// take is true when the card offers "Take it" (D68, M2): the item is with the person's team, or with
// nobody and the step lets them take it.
func (v *view) next(p *playbook.Playbook, it workitem.Item) (bucket int, hint string, take bool, err error) {
	s := v.c.session
	involved := it.CreatedBy == s.PersonID || it.AssigneeID == s.PersonID
	elsewhere := func(h string) (int, string, bool, error) {
		if involved {
			return waitingOnOthers, h, false, nil
		}
		return otherOpen, h, false, nil
	}

	if a := p.Approvals; a != nil && it.State == a.In {
		st, err := workitem.Approvals(v.ctx, v.tx, it.ID)
		if err != nil {
			return 0, "", false, err
		}
		if canDecide(st, s) != nil {
			if it.Reminders > 0 {
				return needsApproval, "Your approval is needed · Seyal reminded you", false, nil
			}
			return needsApproval, "Your approval is needed", false, nil
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
			return 0, "", false, err
		}
		return elsewhere(h)
	}

	members, err := v.members()
	if err != nil {
		return 0, "", false, err
	}
	// Who should act now: the assignee, the team, or (in processes without owners) whoever started it.
	// Work nobody has at all (Seyal started it, or its starter left) may be taken by anyone the step
	// lets receive it (workitem.Free).
	nobody := it.AssigneeID == "" && it.Team == ""
	mine := it.AssigneeID == s.PersonID || (nobody && it.CreatedBy == s.PersonID)
	team := it.AssigneeID == "" && it.Team != "" && slices.Contains(s.Roles, it.Team)
	free := nobody && !mine && workitem.MayTake(p, it, v.actor(), members)
	if v.c.admin() && !mine && workitem.CouldNotEscalate(p, it, members) {
		// Seyal wanted to hand it on but nobody could receive it: admins pass it on (D75, M6), or take
		// it themselves if they may.
		v.flags[it.ID] = flagEscalate
		take = (team || free) && workitem.MayTake(p, it, v.actor(), members)
		return yourTeam, v.couldNotEscalate(p, it), take, nil
	}
	if !mine && !team && !free {
		if v.c.admin() && workitem.NobodyCanTake(p, it, members) {
			v.flags[it.ID] = flagNobody
			return yourTeam, "Nobody can take this", false, nil // admins pass it on (D68, choice 13)
		}
		if v.c.admin() {
			// Its holder left or lost the step's role: admins pass it on (D75, M3).
			h, flag, err := v.stranded(p, it, members)
			if err != nil || h != "" {
				v.flags[it.ID] = flag
				return yourTeam, h, false, err
			}
		}
		return elsewhere("With " + v.holder(it))
	}
	take = (team || free) && workitem.MayTake(p, it, v.actor(), members)
	if role := v.passOnly(p, it); role != "" {
		// They have it but can't work this step (not in its team): all they may do is pass it on.
		h := "Only people in " + role + " do this step: pass it on"
		switch {
		case it.DueOn != "" && it.DueOn < v.today:
			return overdue, h, false, nil
		case team:
			return yourTeam, h, false, nil // escalated to their team (D75, decision 9)
		}
		return yourTurn, h, false, nil
	}
	hint = "Your turn"
	if toAsk := engine.ToAsk(p, it.State, it.Fields); len(toAsk) > 0 {
		for _, f := range p.Fields {
			if f.Key == toAsk[0] {
				hint = "Seyal is asking: " + cmp.Or(f.Ask, ask.Label(f))
			}
		}
	} else if len(engine.Missing(p, it.State, it.Fields)) == 0 {
		mayTake := engine.ActionsFor(p, it.Engine(), v.actor()) // name only moves this person may take
		for _, t := range p.Transitions {
			if t.When == "required_filled" && t.By == "" && slices.Contains(t.From, it.State) && slices.Contains(mayTake, t.Action) {
				hint = fmt.Sprintf("Ready: say %q", strings.ReplaceAll(t.Action, "_", " "))
				break
			}
		}
	}
	// The note given with the latest hand-over says why it came, until it changes hands again.
	if it.HandoffNote != "" {
		hint = it.HandoffNote
		if from := v.names[it.HandoffFrom]; from != "" {
			hint = "From " + from + ": " + it.HandoffNote
		}
	}
	if h := v.chased(p, it); h != "" {
		hint = h
	}
	switch {
	case it.DueOn != "" && it.DueOn < v.today:
		return overdue, hint, take, nil
	case mine:
		return yourTurn, hint, take, nil
	}
	return yourTeam, hint, take, nil
}

// passOnly names the step's team when the person looking has the item but may not work its step: it
// is owned by a role they don't have (an admin passed it to them, or they lost the role), so all they
// can do is pass it on (D68, choice 22). The same goes for work Seyal escalated to their team in
// another team's step (D75, decision 9). Otherwise "". Admins may do everything, so it is "" for them.
func (v *view) passOnly(p *playbook.Playbook, it workitem.Item) string {
	a := v.actor()
	if a.Admin || !engine.Has(it.Engine(), a.Person) || engine.IsHolder(p, it.Engine(), a.Person) {
		return ""
	}
	if kind, role := engine.Owner(p, it.State); kind == "role" {
		return cmp.Or(v.roles[role], role)
	}
	return ""
}

// chased is the card's word about Seyal's chasing in this step stay (D75, M6), or "": that Seyal
// escalated it (while that is still the latest hand-over), or that Seyal reminded whoever has it since
// they got it. The days count to the real today, and are never fewer than the step's wait (only a
// pretend day in `dev chase --as-if` could make them fewer).
func (v *view) chased(p *playbook.Playbook, it workitem.Item) string {
	remind, escalate := p.Steps[it.State].Timing()
	step := sentence(it.State)
	switch {
	case it.EscalatedTo():
		return fmt.Sprintf("Seyal escalated it: waited %s in %s", workingDays(escalate), step)
	case it.EscalatedAt != "" || it.Reminders == 0 || it.RemindedAt < it.HeldSince:
		return ""
	}
	least := remind
	if it.Reminders > 1 {
		least = escalate // the second reminder comes for a new holder, once the escalation is due
	}
	since, err := time.Parse(workitem.HeldSinceLayout, it.StepSince)
	if err != nil {
		return ""
	}
	who := "you" // with the person looking (or with them as its starter, while nobody else has it)
	if it.Team != "" || (it.AssigneeID == "" && it.CreatedBy != v.c.session.PersonID) {
		who = "the team" // a team's work, or work nobody has: everyone who may take it was reminded
	}
	return fmt.Sprintf("Seyal reminded %s · %s in this step", who,
		workingDays(max(v.c.tenant.Calendar.WorkingDays(since, time.Now()), least)))
}

// couldNotEscalate is the admins' hint on work Seyal couldn't hand on: "Couldn't escalate: Owner has
// nobody who can take work".
func (v *view) couldNotEscalate(p *playbook.Playbook, it workitem.Item) string {
	kind, name, _ := strings.Cut(p.Steps[it.State].EscalateTo, ":")
	if kind == "field" {
		label := name
		for _, f := range p.Fields {
			if f.Key == name && f.Label != "" {
				label = f.Label
			}
		}
		return "Couldn't escalate: the " + label + " can't take work"
	}
	return "Couldn't escalate: " + cmp.Or(v.roles[name], name) + " has nobody who can take work"
}

// workingDays says a number of working days: "1 working day", "2 working days".
func workingDays(n int) string {
	if n == 1 {
		return "1 working day"
	}
	return fmt.Sprintf("%d working days", n)
}

// members are the business's active people with a login, read once per page.
func (v *view) members() ([]workitem.Member, error) {
	if v.active == nil {
		list, err := workitem.Members(v.ctx, v.tx)
		if err != nil {
			return nil, err
		}
		v.active = &list
	}
	return *v.active, nil
}

// actor is the person looking, as the process's who words see them.
func (v *view) actor() engine.Actor {
	return workitem.Actor(v.c.session.PersonID, v.c.session.Roles)
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
