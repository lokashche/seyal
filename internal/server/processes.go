// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/notify"
	"seyal/internal/playbook"
	"seyal/internal/send"
	"seyal/internal/start"
)

// Settings → Processes: each business manages its own processes (playbooks). A process is shown in
// plain words; saving a changed definition makes a new version that is live at once, without a
// restart. Work already started keeps the version it started with (D13, D36).

type processList struct {
	InUse     []processRow
	Templates []processRow // shipped templates the business doesn't have yet
	DraftOn   bool         // AI drafting is switched on
	Gap       *addGap      // a template that can't be added until the business's lists get its details
	Leaves    leavesView   // "Leaves Seyal": what goes outside, with a fast stop (design 2.4)
}

// leavesView is the "Leaves Seyal" part at the bottom of Settings → Processes (D89): for now, phone
// notes for this business, with the admin's Pause and Resume. Pausing needs no restart and no new
// set-up file; it is a business.updated ledger event by the admin.
type leavesView struct {
	Server   bool   // the server has phone notes switched on (SEYAL_PUSH=on)
	File     bool   // the set-up file turns them on (notifications: on)
	Hours    string // its working hours for them, "09:00-19:00"
	Paused   bool
	PausedBy string // the admin's name
	PausedAt string // "at 11:02", or "on 5 Nov at 11:02"
	// Other apps (D95): the server's switch, one line per other app, and a test just sent.
	SendOn bool
	Apps   []appLine
	Gone   []failedSend // failed sends to apps the set-up file no longer lists
	Test   *testView
}

// addGap is the advice when a template needs details the business's lists don't have (D75, R), for
// example "Yearly review needs details your lists don't have yet: Members → Wants a review. [Add it and
// use version 0.1]".
type addGap struct {
	ID, Name, Version string
	Gaps              []listGap
}

type processRow struct {
	ID, Name, Description, Version string
	Active                         bool
	Summary                        string
}

type processView struct {
	processRow
	Numbering, States string
	Fields            []processField
	Actions           []string
	Approvals         []string
	OnState           []string
	Deadlines         []string
	Share             []string // the process's own share messages in plain words (D92)
	Versions          []playbook.VersionInfo
	CurrentID         string
	Source            string        // the definition, for advanced editing
	Template          string        // a newer template version, if the shipped one differs
	ListGaps          []listGap     // details the template version needs that the business's lists lack
	DraftOn           bool          // AI drafting is switched on
	Schedule          *scheduleLine // how the process starts on its own now, if it does (D75, R)
	// ScheduleRule is the schedule's rule in plain words, without live counts, or "" for a process that
	// doesn't start on its own. It is in the details and under "What changes" of a draft, so an admin
	// sees before saving that a version starts work on its own.
	ScheduleRule string
	// TemplateSchedule is the newer template version's rule, when it differs from this version's
	// ("" when it doesn't start on its own: TemplateNoSchedule says whether this version does and it won't).
	TemplateSchedule   string
	TemplateNoSchedule bool
}

// scheduleLine is the one line Settings shows for a process that starts on its own, for example "Starts
// on its own: yearly on the 1st, for members with “Wants a review” ticked (2 now). Next run: 1 Apr 2027."
// Warn is set when a run is held back or the schedule can't run.
type scheduleLine struct {
	Text string
	Warn bool
}

// listGap is one of the business's lists that lacks details a template version needs.
type listGap struct {
	Kind, List string
	Details    []string
}

type processField struct {
	Label, Type, Ask string
	Required, Fixed  bool
}

func (s *Server) processesPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showProcesses(w, r, c, "", http.StatusOK, "", "")
}

func (s *Server) processPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showProcesses(w, r, c, r.PathValue("id"), http.StatusOK, "", "")
}

// showProcesses renders the list (id == "") or one process, with an optional notice or problem.
// draft, when given, is shown in the editor instead of the saved definition (after a failed save).
func (s *Server) showProcesses(w http.ResponseWriter, r *http.Request, c *call, id string, status int, notice, problem string, draft ...string) {
	s.showProcessesWith(w, r, c, id, status, notice, problem, nil, nil, draft...)
}

// showProcessesWith is showProcesses, with advice about a template's missing list details on the list,
// or with what a test to another app just sent (D95).
func (s *Server) showProcessesWith(w http.ResponseWriter, r *http.Request, c *call, id string, status int, notice, problem string,
	gap *addGap, test *testView, draft ...string) {
	sv := settingsView{Tab: "processes", Tabs: s.tabs(c, "processes")}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		inUse, err := playbook.ListInUse(r.Context(), tx)
		if err != nil {
			return err
		}
		if id == "" {
			sv.Processes = s.processList(inUse)
			sv.Processes.Gap = gap
			sv.Processes.Leaves, err = s.leavesFor(r, tx, c, inUse)
			sv.Processes.Leaves.Test = test
			return err
		}
		i := slices.IndexFunc(inUse, func(u playbook.InUse) bool { return u.PlaybookID == id })
		if i < 0 {
			return errNoTab
		}
		lib, err := playbook.ForBusiness(r.Context(), tx, s.lib)
		if err != nil {
			return err
		}
		sv.Process = s.processView(inUse[i], lib)
		sv.Process.DraftOn = s.draftOn()
		if t := s.lib.Playbooks[id]; t != nil && sv.Process.Template != "" {
			sv.Process.ListGaps = s.listGaps(t, lib.Masters)
		}
		if sv.Process.Schedule, err = s.scheduleLine(r, tx, c, inUse[i], lib); err != nil {
			return err
		}
		if len(draft) > 0 {
			sv.Process.Source = draft[0]
		}
		return nil
	})
	if errors.Is(err, errNoTab) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := c.page(sv)
	p.Notice, p.Error = notice, problem
	s.render(w, r, status, "settings", p)
}

// listGaps lists, for each list template t needs, the details the template has and the business's own
// list (in kinds) lacks. A list the business doesn't have at all isn't a gap: it is copied whole.
func (s *Server) listGaps(t *playbook.Playbook, kinds map[string]*playbook.MasterKind) []listGap {
	var gaps []listGap
	for _, kind := range playbook.NeededKinds(t) {
		tm := s.lib.Masters[kind]
		if missing := playbook.MissingDetails(kinds[kind], tm); len(missing) > 0 {
			g := listGap{Kind: kind, List: listLabel(tm) + "s"}
			for _, f := range missing {
				g.Details = append(g.Details, ask.Label(f))
			}
			gaps = append(gaps, g)
		}
	}
	return gaps
}

// addListDetails adds to the business's lists the details template t's lists have and theirs lack,
// keeping the business's own changes to those lists.
func (s *Server) addListDetails(r *http.Request, tx pgx.Tx, t *playbook.Playbook, by ledger.Actor) error {
	kinds, err := playbook.Kinds(r.Context(), tx)
	if err != nil {
		return err
	}
	for _, kind := range playbook.NeededKinds(t) {
		missing := playbook.MissingDetails(kinds[kind], s.lib.Masters[kind])
		if len(missing) == 0 {
			continue
		}
		merged, err := kinds[kind].WithDetails(missing)
		if err != nil {
			return err
		}
		if _, err := playbook.SaveKind(r.Context(), tx, merged, by); err != nil {
			return err
		}
	}
	return nil
}

// scheduleLine describes how a process starts on its own (D75, R), or is nil for one that doesn't.
func (s *Server) scheduleLine(r *http.Request, tx pgx.Tx, c *call, u playbook.InUse, lib *playbook.Library) (*scheduleLine, error) {
	if u.Playbook.Schedule == nil {
		return nil, nil
	}
	p, ok, err := start.For(r.Context(), tx, u.PlaybookID, time.Now().In(c.tenant.Zone))
	if err != nil || !ok {
		return nil, err
	}
	if p.Problem != "" {
		return &scheduleLine{Warn: true, Text: "Schedule stopped: " + p.Problem + ". Nothing starts on its own until the list has it again."}, nil
	}
	sc := p.Schedule
	entries, detail := human(p.Kind)+"s", human(sc.Where)
	if mk := lib.Masters[p.Kind]; mk != nil {
		entries = strings.ToLower(listLabel(mk)) + "s"
		for _, f := range mk.Fields {
			if f.Key == sc.Where {
				detail = ask.Label(f)
			}
		}
	}
	line := &scheduleLine{Text: fmt.Sprintf("Starts on its own: %s on the %s, for %s with “%s” ticked (%d now).",
		start.Every(sc.Every), ordinal(sc.Day), entries, detail, p.Now)}
	switch {
	case !p.Active:
		line.Text += " Switched off for new work, so it starts nothing now."
	case p.HeldBack():
		line.Warn = true
		line.Text += fmt.Sprintf(" Held back: %d %s match, the limit is %d. Untick some, or raise max_per_run in a new version; the %s run then starts at Seyal's next check.",
			len(p.Entries), entries, sc.Limit(), p.Period.Name)
	case p.Ready && !p.Decided:
		line.Text += " The " + p.Period.Name + " run starts at Seyal's next check."
	case p.Ready && len(p.Waiting()) > 0:
		line.Text += fmt.Sprintf(" The %s run has %d still to start, at Seyal's next checks.", p.Period.Name, len(p.Waiting()))
	default:
		line.Text += " Next run: " + p.Next().RunDay.Format("2 Jan 2006") + "."
	}
	return line, nil
}

// ordinal writes a day of the month as people say it: 1st, 2nd, 3rd, 4th, 11th, 21st.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

func (s *Server) processList(inUse []playbook.InUse) *processList {
	pl := &processList{DraftOn: s.draftOn()}
	has := map[string]bool{}
	for _, u := range inUse {
		has[u.PlaybookID] = true
		pl.InUse = append(pl.InUse, s.row(u.Playbook, u.Active))
	}
	for _, id := range slices.Sorted(maps.Keys(s.lib.Playbooks)) {
		if !has[id] {
			pl.Templates = append(pl.Templates, s.row(s.lib.Playbooks[id], false))
		}
	}
	return pl
}

func (s *Server) row(p *playbook.Playbook, active bool) processRow {
	slots := 0
	if p.Approvals != nil {
		slots = len(p.Approvals.Slots)
	}
	summary := fmt.Sprintf("%d fields · %d states · %d approvals", len(p.Fields), len(p.States), slots)
	if sc := p.Schedule; sc != nil {
		summary += " · starts on its own " + start.Every(sc.Every)
	}
	return processRow{ID: p.ID, Name: sentence(p.ID), Description: p.Description, Version: p.Version, Active: active,
		Summary: summary}
}

// processView describes one process in plain words.
func (s *Server) processView(u playbook.InUse, lib *playbook.Library) *processView {
	p := u.Playbook
	d := describer{p: p, lib: lib}
	pv := &processView{processRow: s.row(p, u.Active), Numbering: p.Numbering, Versions: u.Versions,
		CurrentID: u.Current.ID, Source: u.Current.Source}
	pv.ScheduleRule = d.schedule()
	if t := s.lib.Playbooks[p.ID]; t != nil && t.Version != p.Version {
		pv.Template = t.Version
		if rule := (describer{p: t, lib: lib}).schedule(); rule != pv.ScheduleRule {
			pv.TemplateSchedule, pv.TemplateNoSchedule = rule, rule == ""
		}
	}

	states := make([]string, len(p.States))
	for i, st := range p.States {
		states[i] = human(st)
	}
	pv.States = strings.Join(states, " · ") + " (starts as " + human(p.Initial) + ")"

	for _, f := range p.Fields {
		// As on an item someone starts by hand: the schedule's period field is fixed only on items the
		// schedule started (D80), and the schedule line already says it fills it.
		fixed := engine.Fixed(p, engine.Item{}, f.Key)
		pv.Fields = append(pv.Fields, processField{Label: ask.Label(f), Type: d.kind(f), Ask: f.Ask,
			Required: f.Required, Fixed: fixed})
	}
	for _, t := range p.Transitions {
		from := make([]string, len(t.From))
		for i, st := range t.From {
			from[i] = human(st)
		}
		line := fmt.Sprintf("%s: from %s to %s", sentence(t.Action), strings.Join(from, " or "), human(t.To))
		if t.When == "required_filled" {
			line += ", once every required field is filled"
		}
		if t.Needs == "reason" {
			line += ", with a reason"
		}
		if t.Back {
			line += " (sends work back)"
		}
		if len(t.Who) > 0 {
			line += "; only " + engine.DescribeWho(t.Who, human, d.label)
		}
		pv.Actions = append(pv.Actions, line)
	}
	if a := p.Approvals; a != nil {
		for _, slot := range a.Slots {
			line := fmt.Sprintf("%s (role %s): ", sentence(slot.Slot), slot.Role)
			if slot.RequiredIf == nil {
				line += "always"
			} else {
				line += "when " + d.cond(*slot.RequiredIf)
			}
			pv.Approvals = append(pv.Approvals, line)
		}
		pv.Approvals = append(pv.Approvals, fmt.Sprintf("While %s. All given → %s; refused → %s.",
			human(a.In), human(a.OnComplete), human(a.OnReject)))
	}
	for _, dl := range p.Deadlines {
		pv.Deadlines = append(pv.Deadlines, "Due on "+d.label(dl.DueFrom))
	}
	for _, st := range slices.Sorted(maps.Keys(p.On)) {
		var parts []string
		for _, act := range p.On[st] {
			switch act.Do {
			case "post":
				parts = append(parts, fmt.Sprintf("Seyal says %q", act.Text))
			case "assign":
				parts = append(parts, "assign to "+d.label(act.To))
			case "copy":
				line := "keep " + d.label(act.From) + " in " + d.label(act.To)
				if act.IfEmpty {
					line += " (only the first time)"
				}
				parts = append(parts, line)
			case "start":
				parts = append(parts, "start "+sentence(act.Process)+" as linked work")
			case "tell_parent":
				parts = append(parts, "tell the item it came from: "+human(act.Action))
			case "send":
				// "send Name, Amount to the other app sheet-one" (D96): the app's label and service
				// live in the business's set-up file, Leaves Seyal below shows them.
				labels := make([]string, len(act.Fields))
				for i, f := range act.Fields {
					labels[i] = d.label(f)
				}
				line := "send " + strings.Join(labels, ", ") + " to the other app " + act.To
				if act.Link {
					line += ", with the item's link"
				}
				parts = append(parts, line)
			}
		}
		pv.OnState = append(pv.OnState, "When "+human(st)+": "+strings.Join(parts, "; "))
	}
	for _, m := range p.Share {
		pv.Share = append(pv.Share, d.share(m))
	}
	return pv
}

// share describes one of the process's share messages (D92), with its words as written, for example
// "Ask for the papers (for outside; in studying, with ops; phone and email from Supplier): “Dear
// {supplier}, …”". Every part is in it, so a change shows under a draft's "What changes".
func (d describer) share(m playbook.ShareMessage) string {
	parts := []string{"for " + m.To}
	if len(m.In) > 0 {
		steps := make([]string, len(m.In))
		for i, st := range m.In {
			steps[i] = human(st)
		}
		parts = append(parts, "in "+strings.Join(steps, ", "))
	}
	if m.Contact != "" {
		var list *playbook.MasterKind
		if f := d.field(m.Contact); f != nil && d.lib != nil {
			list = d.lib.Masters[f.Kind]
		}
		if list == nil || list.Contact == nil {
			parts = append(parts, "the "+d.label(m.Contact)+" list names no phone or email yet (Settings → Lists), so the chat link and Email ask whom to send to")
		} else {
			var has []string
			if list.Contact.Phone != "" {
				has = append(has, "phone")
			}
			if list.Contact.Email != "" {
				has = append(has, "email")
			}
			parts = append(parts, strings.Join(has, " and ")+" from "+d.label(m.Contact))
		}
	}
	return fmt.Sprintf("%s (%s): “%s”", strings.TrimSpace(m.Label), strings.Join(parts, "; "), strings.TrimSpace(m.Text))
}

// describer turns playbook parts into plain words, using field labels.
type describer struct {
	p   *playbook.Playbook
	lib *playbook.Library
}

// schedule describes the process's schedule (D75, R) in plain words, without live counts, for example
// "Monthly on the 1st, for the month just ended: for members with “Wants a reminder” ticked, at
// most 30 a run. Fills Period with the period's name. Due by: the 20th of the month after the period."
// It is "" for a process that doesn't start on its own. Every part of the schedule is in it, so a change
// to any part shows under a draft's "What changes".
func (d describer) schedule() string {
	sc := d.p.Schedule
	if sc == nil {
		return ""
	}
	kind := sc.ForEach
	if f := d.field(sc.ForEach); f != nil && f.Kind != "" {
		kind = f.Kind
	}
	entries, detail := human(kind)+"s", human(sc.Where)
	if d.lib != nil {
		if mk := d.lib.Masters[kind]; mk != nil {
			entries = strings.ToLower(listLabel(mk)) + "s"
			for _, f := range mk.Fields {
				if f.Key == sc.Where {
					detail = ask.Label(f)
				}
			}
		}
	}
	when := "just ended"
	if sc.Period == "current" {
		when = "just starting"
	}
	line := fmt.Sprintf("%s on the %s, for the %s %s: for %s with “%s” ticked, at most %d a run.",
		capital(start.Every(sc.Every)), ordinal(sc.Day), sc.Every, when, entries, detail, sc.Limit())
	if sc.PeriodField != "" {
		line += " Fills " + d.label(sc.PeriodField) + " with the period's name."
	}
	if due := sc.Due; due.Field != "" {
		on := "the last day"
		if due.Day != "last" {
			on = "the " + ordinal(due.DueDay())
		}
		switch due.MonthsAfter {
		case 0:
			on += " of the period's last month"
		case 1:
			on += " of the month after the period"
		default:
			on += fmt.Sprintf(", %d months after the period", due.MonthsAfter)
		}
		line += " " + d.label(due.Field) + ": " + on + "."
	}
	return line
}

func (d describer) field(key string) *playbook.Field {
	for i := range d.p.Fields {
		if d.p.Fields[i].Key == key {
			return &d.p.Fields[i]
		}
	}
	return nil
}

// label names a reference by its labels: a field ("start_date" → its label "Start date"), or a
// field of the chosen master item ("supplier.is_new" → "Supplier → New supplier").
func (d describer) label(ref string) string {
	key, sub, dotted := strings.Cut(ref, ".")
	f := d.field(key)
	name := human(key)
	if f != nil {
		name = ask.Label(*f)
	}
	if !dotted {
		return name
	}
	subName := human(sub)
	if f != nil && d.lib != nil {
		if mk := d.lib.Masters[f.Kind]; mk != nil {
			for _, mf := range mk.Fields {
				if mf.Key == sub && mf.Label != "" {
					subName = mf.Label
				}
			}
		}
	}
	return name + " → " + subName
}

func (d describer) kind(f playbook.Field) string {
	switch f.Type {
	case "choice":
		return "one of " + strings.Join(f.Options, " / ")
	case "list":
		return "list of " + f.Of
	case "master":
		if d.lib != nil {
			if mk := d.lib.Masters[f.Kind]; mk != nil && mk.Label != "" {
				return mk.Label + " (from master data)"
			}
		}
		return human(f.Kind) + " (from master data)"
	case "bool":
		return "yes / no"
	}
	return f.Type
}

func (d describer) cond(c playbook.Condition) string {
	join := func(list []playbook.Condition, word string) string {
		parts := make([]string, len(list))
		for i, sub := range list {
			parts[i] = d.cond(sub)
		}
		return strings.Join(parts, word)
	}
	switch {
	case len(c.Any) > 0:
		return join(c.Any, ", or ")
	case len(c.All) > 0:
		return join(c.All, ", and ")
	}
	var op string
	var v *playbook.Value
	switch {
	case c.Filled != nil && *c.Filled:
		return d.label(c.Field) + " is filled in"
	case c.Filled != nil:
		return d.label(c.Field) + " is empty"
	case c.Eq != nil:
		op, v = "is", c.Eq
	case c.Ne != nil:
		op, v = "is not", c.Ne
	case c.Lt != nil:
		op, v = "is less than", c.Lt
	case c.Gt != nil:
		op, v = "is more than", c.Gt
	default:
		return "?"
	}
	right := d.literal(v.Literal)
	if v.Field != "" {
		right = d.label(v.Field)
	}
	return d.label(c.Field) + " " + op + " " + right
}

func (d describer) literal(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// errListGap stops adding a template whose lists' details the business lacks; the page offers to add them.
var errListGap = errors.New("the lists lack details the template needs")

// addProcess starts using a shipped template, or drafts a new process from plain words.
func (s *Server) addProcess(w http.ResponseWriter, r *http.Request, c *call) {
	if r.PostFormValue("action") != "" {
		s.draftProcess(w, r, c, "")
		return
	}
	t := s.lib.Playbooks[r.PostFormValue("template")]
	if t == nil {
		http.NotFound(w, r)
		return
	}
	var gap *addGap
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		by := ledger.PersonActor(c.session.PersonID)
		if r.PostFormValue("lists") == "add" {
			// The admin pressed "Add them and use version ...": first the details the lists lack.
			if err := s.addListDetails(r, tx, t, by); err != nil {
				return err
			}
		}
		if err := playbook.EnsureKinds(r.Context(), tx, s.lib, t, by); err != nil {
			return err
		}
		if err := s.checkForBusiness(r, tx, t); err != nil {
			kinds, kerr := playbook.Kinds(r.Context(), tx)
			if gaps := s.listGaps(t, kinds); kerr == nil && len(gaps) > 0 {
				// The business's own lists lack details the template's have (D75, R): offer to add them.
				gap = &addGap{ID: t.ID, Name: sentence(t.ID), Version: t.Version, Gaps: gaps}
				return errListGap
			}
			return err
		}
		if err := playbook.EnsureStarted(r.Context(), tx, s.lib, t, by); err != nil {
			return err
		}
		_, err := playbook.EnsureUsed(r.Context(), tx, t, by)
		return err
	})
	if errors.Is(err, errListGap) {
		// The banner at the top says so too: the advice itself is further down, perhaps out of sight.
		s.showProcessesWith(w, r, c, "", http.StatusUnprocessableEntity, "",
			gap.Name+" needs details your lists don't have yet. See below to add them.", gap, nil)
		return
	}
	if err != nil {
		s.showProcesses(w, r, c, "", http.StatusUnprocessableEntity, "", capital(err.Error())+".")
		return
	}
	s.showProcesses(w, r, c, t.ID, http.StatusOK, sentence(t.ID)+" is now in use.", "")
}

// changeProcess saves a new version, switches versions, or switches the process on or off.
func (s *Server) changeProcess(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	if a := r.PostFormValue("action"); a == "draft" || a == "recheck" || r.PostFormValue("fromdraft") != "" {
		s.draftProcess(w, r, c, id)
		return
	}
	by := ledger.PersonActor(c.session.PersonID)
	var notice string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		switch r.PostFormValue("action") {
		case "enable", "disable":
			on := r.PostFormValue("action") == "enable"
			notice = "Switched off for new work. Work already started carries on."
			if on {
				notice = "Switched on."
				inUse, err := playbook.ListInUse(r.Context(), tx)
				if err != nil {
					return err
				}
				for _, u := range inUse {
					if u.PlaybookID == id {
						if err := checkVersionSends(r, tx, u.Current.ID); err != nil {
							return err
						}
					}
				}
			}
			return playbook.SetActive(r.Context(), tx, id, on, by)
		case "use":
			notice = "New work now starts on the chosen version."
			if err := checkVersionSends(r, tx, r.PostFormValue("version")); err != nil {
				return err
			}
			return playbook.Use(r.Context(), tx, r.PostFormValue("version"), by)
		case "template", "template_lists":
			t := s.lib.Playbooks[id]
			if t == nil {
				return errors.New("there is no template for this process")
			}
			if r.PostFormValue("action") == "template_lists" {
				// First add the details the template's lists have and the business's lack, keeping
				// the business's own changes to those lists.
				if err := s.addListDetails(r, tx, t, by); err != nil {
					return err
				}
			}
			if err := playbook.EnsureKinds(r.Context(), tx, s.lib, t, by); err != nil {
				return err
			}
			if err := s.checkForBusiness(r, tx, t); err != nil {
				return s.explainListGap(r, tx, t, err)
			}
			if err := playbook.EnsureStarted(r.Context(), tx, s.lib, t, by); err != nil {
				return err
			}
			versionID, _, err := playbook.Register(r.Context(), tx, t)
			if err != nil {
				return err
			}
			if err := playbook.Use(r.Context(), tx, versionID, by); err != nil {
				return err
			}
			notice = "New work now starts on template version " + t.Version + "."
			behind, err := s.startedBehind(r, tx, t)
			notice += behind
			return err
		case "save":
			p, err := playbook.Parse([]byte(strings.ReplaceAll(r.PostFormValue("source"), "\r\n", "\n")))
			if err != nil {
				return err
			}
			if p.ID != id {
				return fmt.Errorf("the id must stay %q (a different id is a different process)", id)
			}
			if err := s.checkForBusiness(r, tx, p); err != nil {
				return err
			}
			versionID, isNew, err := playbook.Register(r.Context(), tx, p)
			if err != nil {
				return err
			}
			notice = "Version " + p.Version + " saved and in use for new work. Work already started keeps its version."
			if !isNew {
				notice = "Version " + p.Version + " is in use for new work."
			}
			return playbook.Use(r.Context(), tx, versionID, by)
		}
		return errors.New("unknown action")
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "give it a new version") {
			msg = "This version is already used with different content, so old work can always show its exact rules. Change the version line (for example to the next number) and save again"
		}
		var draft []string
		if r.PostFormValue("action") == "save" {
			draft = append(draft, r.PostFormValue("source")) // keep what was typed
		}
		s.showProcesses(w, r, c, id, http.StatusUnprocessableEntity, "", capital(msg)+".", draft...)
		return
	}
	s.showProcesses(w, r, c, id, http.StatusOK, notice, "")
}

// startedBehind says which processes t starts as linked work are on another version than their
// template. Switching one process to its template doesn't switch the ones it starts, because the
// business may have changed them on purpose; the notice says so, so the admin can switch them too.
// It returns "" when there are none.
func (s *Server) startedBehind(r *http.Request, tx pgx.Tx, t *playbook.Playbook) (string, error) {
	inUse, err := playbook.ListInUse(r.Context(), tx)
	if err != nil {
		return "", err
	}
	var notes []string
	for _, id := range playbook.StartsProcesses(t) {
		tmpl := s.lib.Playbooks[id]
		if tmpl == nil {
			continue
		}
		for _, u := range inUse {
			if u.PlaybookID == id && u.Current.Version != tmpl.Version {
				notes = append(notes, fmt.Sprintf(" It starts %s, which is still on version %s; its template is version %s. Switch that one to its template too if you want both to work together as the templates do.",
					sentence(id), u.Current.Version, tmpl.Version))
			}
		}
	}
	return strings.Join(notes, ""), nil
}

// checkForBusiness checks a process against the business's own lists, and its sends against the
// business's other apps (D96).
func (s *Server) checkForBusiness(r *http.Request, tx pgx.Tx, p *playbook.Playbook) error {
	lib, err := playbook.ForBusiness(r.Context(), tx, s.lib)
	if err != nil {
		return err
	}
	if err := lib.Check(p); err != nil {
		return err
	}
	return checkSends(r, tx, p)
}

// checkSends refuses a process whose `do: send` goes to an other app the business's set-up file doesn't
// list, or with fields (or the link) that app may not receive (D96): checked whenever a version is
// switched on for new work.
func checkSends(r *http.Request, tx pgx.Tx, p *playbook.Playbook) error {
	if len(send.Sends(p)) == 0 {
		return nil
	}
	listed, err := send.Targets(r.Context(), tx)
	if err != nil {
		return err
	}
	targets := make([]send.Target, len(listed))
	for i, l := range listed {
		targets[i] = l.Target
	}
	if problems := send.CheckProcess(targets, p); len(problems) > 0 {
		return &playbook.ValidationError{Name: fmt.Sprintf("process %q", p.ID), Problems: problems}
	}
	return nil
}

// checkVersionSends is checkSends for a stored version (by its id), before it is switched on.
func checkVersionSends(r *http.Request, tx pgx.Tx, versionID string) error {
	var source string
	switch err := tx.QueryRow(r.Context(), `SELECT source FROM playbook_versions WHERE tenant_id = current_tenant() AND id::text = $1`,
		versionID).Scan(&source); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil // not a version here: playbook.Use says so in plain words
	case err != nil:
		return err
	}
	p, err := playbook.Parse([]byte(source))
	if err != nil {
		return err
	}
	return checkSends(r, tx, p)
}

// explainListGap turns a failed check of a template version into advice, when the reason is that the
// business's lists lack details the template's lists have (e.g. a detail added in a newer sample).
func (s *Server) explainListGap(r *http.Request, tx pgx.Tx, t *playbook.Playbook, err error) error {
	kinds, kerr := playbook.Kinds(r.Context(), tx)
	if kerr != nil {
		return err
	}
	var gaps, lists []string
	for _, kind := range playbook.NeededKinds(t) {
		mine, tmpl := kinds[kind], s.lib.Masters[kind]
		if mine == nil || tmpl == nil {
			continue
		}
		for _, f := range tmpl.Fields {
			if !slices.ContainsFunc(mine.Fields, func(g playbook.Field) bool { return g.Key == f.Key }) {
				gaps = append(gaps, listLabel(tmpl)+" → "+ask.Label(f))
				if !slices.Contains(lists, listLabel(tmpl)+"s") {
					lists = append(lists, listLabel(tmpl)+"s")
				}
			}
		}
	}
	if len(gaps) == 0 {
		return err
	}
	return fmt.Errorf("version %s needs details your lists don't have yet: %s. Open Settings → Lists → %s and press \"Use the template's definition\", then try again",
		t.Version, strings.Join(gaps, ", "), strings.Join(lists, ", "))
}

// leavesFor reads the "Leaves Seyal" part. inUse are the business's processes, for the fields' labels.
func (s *Server) leavesFor(r *http.Request, tx pgx.Tx, c *call, inUse []playbook.InUse) (leavesView, error) {
	ns, err := notify.LoadSettings(r.Context(), tx)
	if err != nil {
		return leavesView{}, err
	}
	lv := leavesView{Server: s.pushKey != "", File: ns.On, Hours: ns.Hours.String(), Paused: ns.Paused(), SendOn: s.sendOn}
	if lv.Apps, err = s.appLines(r.Context(), tx, c, inUse); err != nil {
		return lv, err
	}
	if lv.Gone, err = goneApps(r.Context(), tx); err != nil {
		return lv, err
	}
	if lv.Paused {
		if err := tx.QueryRow(r.Context(), `SELECT display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			ns.PausedBy).Scan(&lv.PausedBy); err != nil {
			return lv, err
		}
		at := ns.PausedAt.In(c.tenant.Zone)
		lv.PausedAt = at.Format("at 15:04")
		if at.Format(time.DateOnly) != time.Now().In(c.tenant.Zone).Format(time.DateOnly) {
			lv.PausedAt = at.Format("on 2 Jan at 15:04")
		}
	}
	return lv, nil
}

// pausePhoneNotes is the admin's [Pause] or [Resume] for this business's phone notes (D89). It works
// at once: the reader looks at the pause before every pass.
func (s *Server) pausePhoneNotes(w http.ResponseWriter, r *http.Request, c *call) {
	pause := r.PostFormValue("do") == "pause"
	if !pause && r.PostFormValue("do") != "resume" {
		s.showProcesses(w, r, c, "", http.StatusUnprocessableEntity, "", "Please press Pause or Resume.")
		return
	}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		_, err := notify.Pause(r.Context(), tx, pause, c.session.PersonID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	notice := "Phone notes for this business are paused. Nobody gets one until you press Resume."
	if !pause {
		notice = "Phone notes for this business are on again. What happened while they were paused isn't sent."
	}
	s.showProcesses(w, r, c, "", http.StatusOK, notice, "")
}
