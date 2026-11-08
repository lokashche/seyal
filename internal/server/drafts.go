// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/draft"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Describing a process in plain words: AI drafts the definition, Seyal checks it and shows it in plain
// words with what changes, and the admin saves it (or edits the text and checks again). Nothing is
// saved without the admin pressing Save (D43).

type draftView struct {
	ForID    string // the process being changed; "" for a new one
	ForName  string
	Words    string
	Summary  string
	Changes  []string     // "+ …" added, "− …" removed, compared with the current version
	Preview  *processView // nil while the draft has problems
	Source   string
	Lists    []draftList
	Problems []string
}

type draftList struct {
	Kind, Label, Source string
	New                 bool
	Fields              []processField
}

// draftOn reports whether AI drafting is switched on.
func (s *Server) draftOn() bool {
	_, on := s.ai.For(ai.Draft)
	return on
}

// draftProcess handles the forms on the draft page: "draft" asks AI, "recheck" checks edited text,
// "save" saves it. id is the process being changed, or "" for a new one.
func (s *Server) draftProcess(w http.ResponseWriter, r *http.Request, c *call, id string) {
	action := r.PostFormValue("action")
	words := r.PostFormValue("words")
	var dv *draftView
	var saved, problem string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		current, err := s.currentProcess(r.Context(), tx, id)
		if err != nil {
			return err
		}
		var res draft.Result
		switch action {
		case "draft":
			res, err = draft.Process(r.Context(), tx, s.ai, s.lib, current, words, c.session.PersonID)
			switch {
			case errors.Is(err, draft.ErrOff):
				return err
			case err != nil:
				// Keep the transaction, so the failed call stays in AI usage; show the problem instead.
				problem = "AI couldn't draft this just now: " + err.Error()
				res = draft.Result{}
			}
		case "recheck", "save":
			res = draft.Check(r.Context(), tx, current, r.PostFormValue("source"), r.PostForm["list"])
		default:
			return errors.New("unknown action")
		}
		if action == "save" && res.OK() {
			if err := s.saveDraft(r.Context(), tx, res, c.session.PersonID); err != nil {
				return err
			}
			saved = res.Playbook.ID
			return nil
		}
		dv, err = s.draftView(r.Context(), tx, id, current, words, res)
		return err
	})
	switch {
	case errors.Is(err, errNoTab):
		http.NotFound(w, r)
	case err != nil:
		s.showProcesses(w, r, c, id, http.StatusUnprocessableEntity, "", capital(err.Error())+".")
	case saved != "":
		s.showProcesses(w, r, c, saved, http.StatusOK, "Saved and in use for new work. Work already started keeps its version.", "")
	default:
		status := http.StatusOK
		if problem != "" || len(dv.Problems) > 0 {
			status = http.StatusUnprocessableEntity
		}
		sv := settingsView{Tab: "processes", Tabs: s.tabs(c, "processes"), Draft: dv}
		p := c.page(sv)
		p.Error = problem
		s.render(w, r, status, "settings", p)
	}
}

// currentProcess returns the current version of a process the business has, or nil for id "".
func (s *Server) currentProcess(ctx context.Context, tx pgx.Tx, id string) (*playbook.Playbook, error) {
	if id == "" {
		return nil, nil
	}
	inUse, err := playbook.ListInUse(ctx, tx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(inUse, func(u playbook.InUse) bool { return u.PlaybookID == id })
	if i < 0 {
		return nil, errNoTab
	}
	return inUse[i].Playbook, nil
}

// saveDraft saves a checked draft: the process version first (so a list change is checked against
// it), then the lists. It is in use for new work at once.
func (s *Server) saveDraft(ctx context.Context, tx pgx.Tx, res draft.Result, personID string) error {
	by := ledger.PersonActor(personID)
	versionID, _, err := playbook.Register(ctx, tx, res.Playbook)
	if err != nil {
		return err
	}
	if err := playbook.Use(ctx, tx, versionID, by); err != nil {
		return err
	}
	for _, mk := range res.Lists {
		if _, err := playbook.SaveKind(ctx, tx, mk, by); err != nil {
			return err
		}
	}
	return nil
}

// draftView shows a draft in plain words, with what changes compared with the current version.
func (s *Server) draftView(ctx context.Context, tx pgx.Tx, id string, current *playbook.Playbook, words string,
	res draft.Result) (*draftView, error) {
	dv := &draftView{ForID: id, ForName: sentence(id), Words: words, Summary: res.Summary, Source: res.Source,
		Problems: res.Problems}
	lib, err := playbook.ForBusiness(ctx, tx, s.lib)
	if err != nil {
		return nil, err
	}
	for _, mk := range res.Lists {
		dl := draftList{Kind: mk.Kind, Label: listLabel(mk), Source: string(mk.Source), New: lib.Masters[mk.Kind] == nil}
		for _, f := range mk.Fields {
			dl.Fields = append(dl.Fields, processField{Label: ask.Label(f), Type: describer{}.kind(f)})
		}
		dv.Lists = append(dv.Lists, dl)
	}
	var before *processView
	if current != nil {
		before = s.processView(playbook.InUse{PlaybookID: current.ID, Active: true, Playbook: current}, lib)
	}
	for _, mk := range res.Lists {
		lib.Masters[mk.Kind] = mk
	}
	if res.Playbook != nil {
		dv.Preview = s.processView(playbook.InUse{PlaybookID: res.Playbook.ID, Active: true, Playbook: res.Playbook}, lib)
		if before != nil {
			dv.Changes = changes(before, dv.Preview)
		}
	}
	return dv, nil
}

// changes lists, in plain words, what is in the draft but not the current version (+) and the other
// way round (−).
func changes(before, after *processView) []string {
	old, now := plainLines(before), plainLines(after)
	var out []string
	for _, l := range old {
		if !slices.Contains(now, l) {
			out = append(out, "− "+l)
		}
	}
	for _, l := range now {
		if !slices.Contains(old, l) {
			out = append(out, "+ "+l)
		}
	}
	return out
}

func plainLines(pv *processView) []string {
	lines := []string{"Description: " + pv.Description, "Numbered " + pv.Numbering, "Steps: " + pv.States}
	if pv.ScheduleRule != "" {
		// Seyal starts work on its own with it, so adding, removing or changing it must show.
		lines = append(lines, "Starts on its own: "+pv.ScheduleRule)
	}
	for _, f := range pv.Fields {
		l := fmt.Sprintf("Detail %s: %s", f.Label, f.Type)
		if f.Required {
			l += ", required"
		}
		if f.Fixed {
			l += ", filled automatically"
		}
		if f.Ask != "" {
			l += fmt.Sprintf(", Seyal asks “%s”", f.Ask)
		}
		lines = append(lines, l)
	}
	for _, group := range []struct {
		name  string
		lines []string
	}{{"Action", pv.Actions}, {"Approval", pv.Approvals}, {"Automatic", pv.OnState}, {"Deadline", pv.Deadlines}, {"Share message", pv.Share}} {
		for _, l := range group.lines {
			lines = append(lines, group.name+": "+strings.TrimSpace(l))
		}
	}
	return lines
}
