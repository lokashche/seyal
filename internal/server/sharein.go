// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/auth"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Share in (Phase 4, part 4b-3, D93). On an Android phone with Seyal installed, a person can pick
// Seyal in another app's Share menu (a chat app's voice note, a photo, a PDF, a message). The phone posts
// the shared things to /t/<slug>/share; Seyal's service worker catches that on the phone, keeps them
// there (IndexedDB) and opens Tell Seyal, which shows a banner (app.js). Nothing reaches the server
// until the person presses Send: then the ordinary, logged-in Tell Seyal form carries the words and
// files, with via=share and the share's own id. The server never takes data from the Share menu's post
// itself: that post can't carry a login, and a form on any website could make the same post.
//
// A message sent that way is marked (messages.via = 'share'): the thread says "shared from another
// app", its words never act (ask/shared.go) and it is kept away from the AI.

// shareInView is what the Tell Seyal banner needs for something shared into Seyal.
type shareInView struct {
	On     bool            // the business allows sharing (its set-up file doesn't say "sharing: off")
	New    bool            // a process is switched on for new work; without one, it can only go into an item
	Items  []pickItem      // "add to an item": the items the person opened lately, then their My Work
	Others []auth.Business // the login's other active businesses, for [change] (only with two or more)
}

// pickItem is one item offered under "add to an item".
type pickItem struct {
	ID, Number, Title string
}

// Label is how the item reads in the picker; resolveItem reads the number back from it.
func (p pickItem) Label() string {
	if p.Title == "" {
		return p.Number
	}
	return p.Number + " · " + p.Title
}

// maxPick is how many items "add to an item" offers before the person types a number or a title.
const maxPick = 20

// shareIn builds the banner's view for Tell Seyal. work is the page's My Work list. The items for "add
// to an item" are listed only when Tell Seyal is the page's own view (?view=tell, where the service
// worker opens a share): My Work's lists don't carry them on every page.
func (v *view) shareIn(work *myWork, items bool) (*shareInView, error) {
	si := &shareInView{On: v.c.tenant.Sharing}
	if !si.On {
		return si, nil
	}
	if v.c.session.AccountID != "" {
		mine, err := auth.Businesses(v.ctx, v.tx, v.c.session.AccountID)
		if err != nil {
			return nil, err
		}
		for _, b := range mine {
			if b.Active && b.Slug != v.c.tenant.Slug {
				si.Others = append(si.Others, b)
			}
		}
	}
	if !items {
		return si, nil
	}
	// Items the person opened lately first (item_seen, D75), then their own My Work.
	rows, _ := v.tx.Query(v.ctx, `
		SELECT item_id::text FROM item_seen WHERE tenant_id = current_tenant() AND person_id::text = $1
		ORDER BY seen_at DESC LIMIT 60`, v.c.session.PersonID)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if work != nil {
		for _, r := range work.Rows {
			if r.Mine {
				ids = append(ids, r.ID)
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || len(si.Items) == maxPick {
			continue
		}
		seen[id] = true
		it, err := workitem.Get(v.ctx, v.tx, id)
		if err != nil {
			return nil, err
		}
		open, err := v.open(it)
		if err != nil {
			return nil, err
		}
		if open {
			si.Items = append(si.Items, pickItem{ID: it.ID, Number: it.Number, Title: strings.TrimSpace(it.Title)})
		}
	}
	return si, nil
}

// open reports whether the item isn't closed (done or cancelled).
func (v *view) open(it workitem.Item) (bool, error) {
	p, err := v.playbook(it)
	if err != nil {
		return false, err
	}
	return !engine.IsClosed(p, it.State), nil
}

// errClosedItem is a shared message for an item that closed since the person picked it.
type errClosedItem struct{ number string }

func (e errClosedItem) Error() string {
	return e.number + " is closed. Pick another item or start new work"
}

// resolveItem finds the open item the person meant under "add to an item": an item id (picked from the
// list), a label from the list ("J-2026-0007 · Sundaram Textiles"), a number ("J-2026-0007"), the end of
// one ("0007"), or part of a title. Any open item the person may see counts, not only the ones listed.
// An item named exactly that has closed is refused with errClosedItem; no match or several are refused
// in plain words.
func (v *view) resolveItem(said string) (workitem.Item, error) {
	said = strings.TrimSpace(said)
	if number, _, ok := strings.Cut(said, " · "); ok {
		said = strings.TrimSpace(number)
	}
	if said == "" {
		return workitem.Item{}, errors.New("pick the item to add this to, or choose new work")
	}
	items, err := workitem.List(v.ctx, v.tx) // open items only
	if err != nil {
		return workitem.Item{}, err
	}
	if items, err = workitem.Visible(v.ctx, v.tx, v.c.session.PersonID, items); err != nil {
		return workitem.Item{}, err
	}
	lower := strings.ToLower(said)
	digits := digitsOnly.MatchString(said)
	var exact, found []workitem.Item
	for _, it := range items {
		number := strings.ToLower(it.Number)
		switch {
		case it.ID == said || number == lower:
			exact = append(exact, it)
		case digits && strings.HasSuffix(number, "-"+lower), digits && len(said) >= 3 && strings.HasSuffix(number, lower),
			!digits && len([]rune(said)) >= 3 && strings.Contains(strings.ToLower(it.Title), lower):
			found = append(found, it)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		numbers := make([]string, 0, 5)
		for _, it := range found[:min(5, len(found))] {
			numbers = append(numbers, it.Number)
		}
		return workitem.Item{}, fmt.Errorf("more than one open item matches %q (%s); type more of the number", said, strings.Join(numbers, ", "))
	}
	// Not open: an item named exactly that closed since the person picked it.
	var number string
	err = v.tx.QueryRow(v.ctx, `SELECT number FROM work_items WHERE tenant_id = current_tenant() AND (id::text = $1 OR lower(number) = $2)`,
		said, lower).Scan(&number)
	switch {
	case err == nil:
		return workitem.Item{}, errClosedItem{number}
	case !errors.Is(err, pgx.ErrNoRows):
		return workitem.Item{}, err
	}
	return workitem.Item{}, fmt.Errorf("no open item matches %q", said)
}

// digitsOnly is the end of an item's number typed alone ("0007").
var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

// shareIDPattern is a share's own id, made on the phone (a UUID).
var shareIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// tellShared sends what was shared into Seyal, after the person pressed Send on the banner: as new work
// (the process the person picked, or Seyal's suggestion by words only, to confirm), or into the item
// they picked. The server builds the message's ExternalID itself, "share:<person>:<share id>", so the
// same share sent twice (a double tap, a retry after a lost answer) is one message (channel.Receive),
// and one sent again to another place is refused. Any refusal answers with an error status: the page
// then keeps the share on the phone.
func (s *Server) tellShared(w http.ResponseWriter, r *http.Request, c *call, in channel.Incoming) {
	if !c.tenant.Sharing {
		s.showScreen(w, r, c, "", http.StatusForbidden, "Sharing into Seyal is off for this business.")
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.FormValue("share_id")))
	if !shareIDPattern.MatchString(id) {
		s.showScreen(w, r, c, "", http.StatusBadRequest, "This share has no id. Share it into Seyal again.")
		return
	}
	in.Via, in.ExternalID, in.Button, in.Spoken = channel.ViaShare, "share:"+c.session.PersonID+":"+id, "", nil
	if in.Text == "" && len(in.Files) == 0 {
		s.showScreen(w, r, c, "", http.StatusUnprocessableEntity, "The share is empty. Discard it.")
		return
	}
	target := strings.TrimSpace(r.FormValue("item"))
	var itemID, arrived string
	var reply ask.Reply
	var routing *ask.Routing
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		if target != "" {
			it, err := v.resolveItem(target)
			if err != nil {
				return err
			}
			itemID = it.ID
			if _, err := ask.Converse(r.Context(), tx, s.env(), it.ID, in); err != nil {
				return err
			}
			return arrivedNow(r.Context(), tx, it.ID, &arrived)
		}
		current, err := v.current()
		if err != nil {
			return err
		}
		if len(current) == 0 {
			return errors.New("no process is switched on yet; add it to an item instead")
		}
		chosen := r.FormValue("playbook")
		if (chosen == "" || chosen == "auto") && len(current) > 1 {
			processes, err := askProcesses(current)
			if err != nil {
				return err
			}
			rt, err := ask.Route(r.Context(), tx, s.env(), processes, in) // no AI for shared words (ask.Route)
			routing = &rt
			return err
		}
		if chosen == "" || chosen == "auto" {
			chosen = current[0].ID
		}
		if !slices.ContainsFunc(current, func(p struct{ ID, Source string }) bool { return p.ID == chosen }) {
			return errors.New("that process isn't in use any more; reload the page")
		}
		reply, err = ask.Begin(r.Context(), tx, s.env(), chosen, in)
		if err == nil && reply.Item.ID == "" && reply.Message.ID != "" && reply.Message.ThreadID == "" {
			// Sent again after Seyal asked which process it is for, and that answer was lost: the
			// saved message starts now, with the process picked this time.
			reply, err = ask.BeginFrom(r.Context(), tx, s.env(), chosen, reply.Message.ID, c.session.PersonID)
		}
		return err
	})
	var closed errClosedItem
	switch {
	case errors.Is(err, channel.ErrRepeatElsewhere):
		s.showScreen(w, r, c, "", http.StatusConflict, "This share was already sent, to another place. Discard it here.")
		return
	case errors.As(err, &closed):
		// 422, not 409: nothing was kept, so the phone drops its "sent to this business" mark (app.js keeps
		// it only for a 409, which means this business may already hold the share).
		s.showScreen(w, r, c, "", http.StatusUnprocessableEntity, capital(closed.Error())+".")
		return
	case err != nil:
		s.problem(w, r, c, "", err)
		return
	case itemID != "":
		s.done(w, r, c, itemID, arrived, nil)
		return
	case routing != nil && routing.Message.ThreadID != "":
		http.Redirect(w, r, "/t/"+c.tenant.Slug+"/items/"+routing.Message.ThreadID, http.StatusSeeOther)
		return
	case routing != nil:
		s.askWhichProcess(w, r, c, *routing)
		return
	case reply.Item.ID == "": // nothing started: an error status, so the phone keeps the share
		s.showScreen(w, r, c, "", http.StatusConflict, "This share could not be started. Pick a process and send it again.")
		return
	}
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/items/"+reply.Item.ID, http.StatusSeeOther)
}

// askProcesses lists the processes for routing.
func askProcesses(current []struct{ ID, Source string }) ([]ask.Process, error) {
	processes := make([]ask.Process, 0, len(current))
	for _, pv := range current {
		p, err := playbook.Parse([]byte(pv.Source))
		if err != nil {
			return nil, err
		}
		processes = append(processes, ask.Process{VersionID: pv.ID, ID: p.ID, Name: sentence(p.ID),
			Description: p.Description, Examples: p.Examples, Fallback: p.Fallback})
	}
	return processes, nil
}

// shareFallback answers the Share menu's post when the service worker didn't catch it (it isn't
// installed yet, or the browser skipped it). It never reads the post: it can't carry a login, and any
// website could send the same form. It only opens Tell Seyal, which says to open Seyal once and share
// again (D93).
func (s *Server) shareFallback(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !slugPattern.MatchString(slug) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/t/"+slug+"/?view=tell&share=missed", http.StatusSeeOther)
}
