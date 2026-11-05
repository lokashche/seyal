// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Pass on and Take it (D68, M1 and M2) on the Work Item side panel and the My Work cards. The panel
// only offers what the person may do; the server checks everything again in workitem.Pass and
// workitem.Take, whichever form or route the request came from.

// hand prepares the panel's "who has it" part for the person looking.
func (v *view) hand(p *playbook.Playbook, it workitem.Item) (*handView, error) {
	members, err := v.members()
	if err != nil {
		return nil, err
	}
	a := v.actor()
	h := &handView{Holder: it.HolderKey(), MaxNote: workitem.MaxNote,
		CanTake: workitem.MayTake(p, it, a, members) &&
			slices.ContainsFunc(members, func(m workitem.Member) bool { return m.ID == a.ID }),
		Nobody: a.Admin && workitem.NobodyCanTake(p, it, members)}
	if ok, _ := workitem.MayPass(it, a); !ok {
		return h, nil
	}
	h.CanPass = true
	list := workitem.Receivers(p, it, members, v.roles)
	for _, r := range list {
		h.To = append(h.To, receiverOption{Key: r.Key(), Name: r.Name, Empty: r.Empty})
	}
	// Give back to the team: the person took it from the team that owns this step.
	if kind, role := engine.Owner(p, it.State); kind == "role" && it.AssigneeID == a.ID {
		if i := slices.IndexFunc(list, func(r workitem.Receiver) bool { return r.Team == role }); i >= 0 {
			h.GiveBack = &receiverOption{Key: list[i].Key(), Name: list[i].Name, Empty: list[i].Empty}
		}
	}
	if a.Admin {
		for _, m := range members {
			if m.ID != it.AssigneeID && !slices.ContainsFunc(list, func(r workitem.Receiver) bool { return r.Person == m.ID }) {
				h.Others = append(h.Others, receiverOption{Key: "person:" + m.ID, Name: m.Name})
			}
		}
	}
	return h, nil
}

// receiverChoices keeps, in the select of the field that says who has the item in this step, only the
// people who may receive it here (and, for an admin, the others they may pass it to), plus whoever the
// field names now, so the select shows it, and whoever has the item, who may put their own name in.
// all is every person, as the field's select lists them.
func receiverChoices(h *handView, holder string, current any, all []option) []option {
	allowed := map[string]bool{}
	if holder != "" {
		allowed[holder] = true
	}
	if h != nil {
		for _, r := range append(slices.Clone(h.To), h.Others...) {
			if id, ok := strings.CutPrefix(r.Key, "person:"); ok {
				allowed[id] = true
			}
		}
	}
	var out []option
	for _, o := range all {
		if allowed[o.Value] || o.Value == current {
			out = append(out, o)
		}
	}
	return out
}

// via is where a hand-over was asked for: the side panel, or a button on a My Work card.
func via(r *http.Request) string {
	if r.PostFormValue("via") == "button" {
		return "button"
	}
	return "panel"
}

// pass handles Pass on (and Give back to the team) from the side panel: POST /items/{id}/pass with
// to ("person:<id>" or "team:<role>"), an optional note, and the holder the page showed.
func (s *Server) pass(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	to := workitem.PassOn{Note: r.PostFormValue("note"), Via: via(r), Seen: r.PostFormValue("holder")}
	// A refusal shows the form again with what the person chose and wrote, so the note isn't lost.
	draft := func(sc *screen) {
		if sc.Item != nil && sc.Item.Hand != nil {
			sc.Item.Hand.Draft = &passDraft{To: r.PostFormValue("to"), Note: to.Note}
		}
	}
	kind, key, _ := strings.Cut(r.PostFormValue("to"), ":")
	switch {
	case kind == "person" && key != "":
		to.Person = key
	case kind == "team" && key != "":
		to.Team = key
	default:
		s.problem(w, r, c, id, errors.New("choose who to pass it to"), draft)
		return
	}
	var notice string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		var err error
		_, notice, err = workitem.Pass(r.Context(), tx, id, to, ledger.PersonActor(c.session.PersonID))
		return err
	})
	switch {
	case err != nil:
		s.problem(w, r, c, id, err, draft)
	case notice != "":
		s.show(w, r, c, id, http.StatusOK, "", notice)
	default:
		s.done(w, r, c, id, nil)
	}
}

// take handles Take it, from the side panel or a My Work card: POST /items/{id}/take with the holder
// the page showed.
func (s *Server) take(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		_, err := workitem.Take(r.Context(), tx, id, r.PostFormValue("holder"), via(r), ledger.PersonActor(c.session.PersonID))
		return err
	})
	s.done(w, r, c, id, err)
}
