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
	Source            string // the definition, for advanced editing
	Template          string // a newer template version, if the shipped one differs
	DraftOn           bool   // AI drafting is switched on
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
		case "template":
			t := s.lib.Playbooks[id]
			if t == nil {
				return errors.New("there is no template for this process")
			}
			if err := playbook.EnsureKinds(r.Context(), tx, s.lib, t, by); err != nil {
				return err
			}
			if err := s.checkForBusiness(r, tx, t); err != nil {
				return err
			}
			versionID, _, err := playbook.Register(r.Context(), tx, t)
			if err != nil {
				return err
			}
			notice = "New work now starts on template version " + t.Version + "."
			return playbook.Use(r.Context(), tx, versionID, by)
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

// checkForBusiness checks a process against the business's own lists.
func (s *Server) checkForBusiness(r *http.Request, tx pgx.Tx, p *playbook.Playbook) error {
	lib, err := playbook.ForBusiness(r.Context(), tx, s.lib)
	if err != nil {
		return err
	}
	return lib.Check(p)
}
