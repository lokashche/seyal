// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
)

// Settings → Lists: each business keeps its own lists (master kinds) and what each entry records.
// They are saved without a restart. The files in the master-data folder are only templates to start
// from.

type listsView struct {
	Lists     []listRow
	Templates []listRow // templates the business doesn't have yet
	Blank     string    // a starting definition for a new list
}

type listRow struct {
	Kind, Label, Summary string
}

type listView struct {
	listRow
	Version int
	Fields  []processField
	UsedBy  []string // processes that use this list
	Source  string
}

const blankList = `kind: member
label: Member
fields:
  - { key: phone, label: Phone, type: text }
  - { key: joined_on, label: Joined on, type: date }
`

func (s *Server) listsPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showLists(w, r, c, "", http.StatusOK, "", "")
}

func (s *Server) listPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showLists(w, r, c, r.PathValue("kind"), http.StatusOK, "", "")
}

// showLists renders all lists (kind == "") or one, with an optional notice or problem. draft, when
// given, is shown in the editor instead of the saved definition (after a failed save).
func (s *Server) showLists(w http.ResponseWriter, r *http.Request, c *call, kind string, status int, notice, problem string, draft ...string) {
	sv := settingsView{Tab: "lists", Tabs: s.tabs(c, "lists")}
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		kinds, err := playbook.Kinds(r.Context(), tx)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		rows, _ := tx.Query(r.Context(), `
			SELECT kind, count(*) FILTER (WHERE active) FROM master_items WHERE tenant_id = current_tenant() GROUP BY kind`)
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			Kind string
			N    int
		}])
		if err != nil {
			return err
		}
		for _, l := range list {
			counts[l.Kind] = l.N
		}

		if kind == "" {
			lv := &listsView{Blank: blankList}
			for _, k := range slices.Sorted(maps.Keys(kinds)) {
				lv.Lists = append(lv.Lists, listRow{Kind: k, Label: listLabel(kinds[k]),
					Summary: fmt.Sprintf("%d details · %d in use", len(kinds[k].Fields), counts[k])})
			}
			for _, k := range slices.Sorted(maps.Keys(s.lib.Masters)) {
				if kinds[k] == nil {
					t := s.lib.Masters[k]
					lv.Templates = append(lv.Templates, listRow{Kind: k, Label: listLabel(t),
						Summary: fmt.Sprintf("%d details", len(t.Fields))})
				}
			}
			sv.Lists = lv
			return nil
		}

		mk := kinds[kind]
		if mk == nil {
			return errNoTab
		}
		var version int
		if err := tx.QueryRow(r.Context(), `SELECT version FROM master_kinds WHERE tenant_id = current_tenant() AND kind = $1`,
			kind).Scan(&version); err != nil {
			return err
		}
		lv := &listView{listRow: listRow{Kind: kind, Label: listLabel(mk),
			Summary: fmt.Sprintf("%d in use", counts[kind])}, Version: version, Source: string(mk.Source)}
		for _, f := range mk.Fields {
			lv.Fields = append(lv.Fields, processField{Label: ask.Label(f), Type: describer{}.kind(f)})
		}
		inUse, err := playbook.ListInUse(r.Context(), tx)
		if err != nil {
			return err
		}
		for _, u := range inUse {
			if slices.Contains(playbook.NeededKinds(u.Playbook), kind) {
				lv.UsedBy = append(lv.UsedBy, sentence(u.PlaybookID))
			}
		}
		if len(draft) > 0 {
			lv.Source = draft[0]
		}
		sv.List = lv
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

func listLabel(mk *playbook.MasterKind) string {
	if mk.Label != "" {
		return mk.Label
	}
	return sentence(mk.Kind)
}

// addList adds a list to the business: a copy of a template, or a new definition.
func (s *Server) addList(w http.ResponseWriter, r *http.Request, c *call) {
	source := r.PostFormValue("source")
	if t := s.lib.Masters[r.PostFormValue("template")]; t != nil {
		source = string(t.Source)
	}
	var kind string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		mk, err := playbook.ParseMasterKind([]byte(strings.ReplaceAll(source, "\r\n", "\n")))
		if err != nil {
			return err
		}
		if has, err := playbook.Kind(r.Context(), tx, mk.Kind); err != nil || has != nil {
			if err == nil {
				err = fmt.Errorf("there is already a list called %q; change it on its own page", mk.Kind)
			}
			return err
		}
		kind = mk.Kind
		_, err = playbook.SaveKind(r.Context(), tx, mk, ledger.PersonActor(c.session.PersonID))
		return err
	})
	if err != nil {
		s.showLists(w, r, c, "", http.StatusUnprocessableEntity, "", capital(err.Error())+".")
		return
	}
	s.showLists(w, r, c, kind, http.StatusOK, "List added. You can add entries to it now.", "")
}

// changeList saves a changed definition of one list.
func (s *Server) changeList(w http.ResponseWriter, r *http.Request, c *call) {
	kind := r.PathValue("kind")
	source := r.PostFormValue("source")
	changed := false
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		if has, err := playbook.Kind(r.Context(), tx, kind); err != nil || has == nil {
			if err == nil {
				err = errNoTab
			}
			return err
		}
		mk, err := playbook.ParseMasterKind([]byte(strings.ReplaceAll(source, "\r\n", "\n")))
		if err != nil {
			return err
		}
		if mk.Kind != kind {
			return fmt.Errorf("the kind must stay %q (a different kind is a different list)", kind)
		}
		changed, err = playbook.SaveKind(r.Context(), tx, mk, ledger.PersonActor(c.session.PersonID))
		return err
	})
	switch {
	case errors.Is(err, errNoTab):
		http.NotFound(w, r)
	case err != nil:
		s.showLists(w, r, c, kind, http.StatusUnprocessableEntity, "", capital(err.Error())+".", source)
	case !changed:
		s.showLists(w, r, c, kind, http.StatusOK, "Nothing changed.", "")
	default:
		s.showLists(w, r, c, kind, http.StatusOK, "Saved. The forms and the chat use it at once.", "")
	}
}
