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

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Settings → Processes: each business manages its own processes (playbooks). A process is shown in
// plain words; saving a changed definition makes a new version that is live at once, without a
// restart. Work already started keeps the version it started with (D13, D36).

type processList struct {
	InUse     []processRow
	Templates []processRow // shipped templates the business doesn't have yet
	DraftOn   bool         // AI drafting is switched on
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
	Versions          []playbook.VersionInfo
	CurrentID         string
	Source            string    // the definition, for advanced editing
	Template          string    // a newer template version, if the shipped one differs
	ListGaps          []listGap // details the template version needs that the business's lists lack
	DraftOn           bool      // AI drafting is switched on
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
	sv := settingsView{Tab: "processes", Tabs: s.tabs(c, "processes")}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		inUse, err := playbook.ListInUse(r.Context(), tx)
		if err != nil {
			return err
		}
		if id == "" {
			sv.Processes = s.processList(inUse)
			return nil
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
			for _, kind := range playbook.NeededKinds(t) {
				tm := s.lib.Masters[kind]
				if missing := playbook.MissingDetails(lib.Masters[kind], tm); len(missing) > 0 {
					g := listGap{Kind: kind, List: listLabel(tm) + "s"}
					for _, f := range missing {
						g.Details = append(g.Details, ask.Label(f))
					}
					sv.Process.ListGaps = append(sv.Process.ListGaps, g)
				}
			}
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
	return processRow{ID: p.ID, Name: sentence(p.ID), Description: p.Description, Version: p.Version, Active: active,
		Summary: fmt.Sprintf("%d fields · %d states · %d approvals", len(p.Fields), len(p.States), slots)}
}

// processView describes one process in plain words.
func (s *Server) processView(u playbook.InUse, lib *playbook.Library) *processView {
	p := u.Playbook
	d := describer{p: p, lib: lib}
	pv := &processView{processRow: s.row(p, u.Active), Numbering: p.Numbering, Versions: u.Versions,
		CurrentID: u.Current.ID, Source: u.Current.Source}
	if t := s.lib.Playbooks[p.ID]; t != nil && t.Version != p.Version {
		pv.Template = t.Version
	}

	states := make([]string, len(p.States))
	for i, st := range p.States {
		states[i] = human(st)
	}
	pv.States = strings.Join(states, " · ") + " (starts as " + human(p.Initial) + ")"

	for _, f := range p.Fields {
		pv.Fields = append(pv.Fields, processField{Label: ask.Label(f), Type: d.kind(f), Ask: f.Ask,
			Required: f.Required, Fixed: f.Readonly})
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
			}
		}
		pv.OnState = append(pv.OnState, "When "+human(st)+": "+strings.Join(parts, "; "))
	}
	return pv
}

// describer turns playbook parts into plain words, using field labels.
type describer struct {
	p   *playbook.Playbook
	lib *playbook.Library
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
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		by := ledger.PersonActor(c.session.PersonID)
		if err := playbook.EnsureKinds(r.Context(), tx, s.lib, t, by); err != nil {
			return err
		}
		if err := s.checkForBusiness(r, tx, t); err != nil {
			return err
		}
		if err := playbook.EnsureStarted(r.Context(), tx, s.lib, t, by); err != nil {
			return err
		}
		_, err := playbook.EnsureUsed(r.Context(), tx, t, by)
		return err
	})
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
			}
			return playbook.SetActive(r.Context(), tx, id, on, by)
		case "use":
			notice = "New work now starts on the chosen version."
			return playbook.Use(r.Context(), tx, r.PostFormValue("version"), by)
		case "template", "template_lists":
			t := s.lib.Playbooks[id]
			if t == nil {
				return errors.New("there is no template for this process")
			}
			if r.PostFormValue("action") == "template_lists" {
				// First add the details the template's lists have and the business's lack, keeping
				// the business's own changes to those lists.
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

// checkForBusiness checks a process against the business's own lists.
func (s *Server) checkForBusiness(r *http.Request, tx pgx.Tx, p *playbook.Playbook) error {
	lib, err := playbook.ForBusiness(r.Context(), tx, s.lib)
	if err != nil {
		return err
	}
	return lib.Check(p)
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
