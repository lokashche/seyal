// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ask"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Moving someone's open work (D75, M3) on the screens that already exist: the list is an answer on
// Tell Seyal (typed or spoken, or opened from Settings → People), and its button posts to /move. The
// list writes nothing; workitem.Reassign checks everything again on the press.

// moveView is the list on Tell Seyal, ready for the page.
type moveView struct {
	*ask.MoveOffer
	Pick   bool           // Pick: every item with a box to tick, and "Move these"
	Shown  []ask.MoveItem // the items listed (at most ask.MoveShown, unless picking)
	More   int            // how many more there are, not listed
	Ticked []string       // without Pick: the ids "Move all" sends (the boxes shown can't be changed)
	Button string         // "Move all 3 to Dev", "Move 2 to Dev", "Move these"; "" when nothing is ticked
	Note   string         // the note typed before Pick, or before a press that was refused
	Max    int            // the longest note, in letters
}

func newMoveView(o *ask.MoveOffer, pick bool) *moveView {
	mv := &moveView{MoveOffer: o, Pick: pick, Shown: o.Items, Max: workitem.MaxNote}
	if len(o.Items) == 0 {
		return mv
	}
	if pick {
		mv.Button = "Move these"
		return mv
	}
	if len(o.Items) > ask.MoveShown {
		mv.Shown, mv.More = o.Items[:ask.MoveShown], len(o.Items)-ask.MoveShown
	}
	for _, it := range o.Items {
		if it.Ticked && len(mv.Ticked) < workitem.MaxMove { // more than one press can move: the rest next time
			mv.Ticked = append(mv.Ticked, it.ID)
		}
	}
	switch n := len(mv.Ticked); {
	case n == 0:
	case n == len(o.Items) && n > 1:
		mv.Button = fmt.Sprintf("Move all %d to %s", n, o.To.Name)
	default:
		mv.Button = fmt.Sprintf("Move %d to %s", n, o.To.Name)
	}
	return mv
}

// moveFromLink fills the list opened from a link: a "which one?" button, or Settings → People
// (?move_from=…&move_to=…, and pick=1 for every item with a box). The link never brings a note or ticked
// boxes: a link anyone can send mustn't put words into the admin's note (it stays in the threads for
// good). Pick and a refused press bring them inside the same request instead (see keepMove).
func (v *view) moveFromLink(t *tellView) error {
	from, to := v.q.Get("move_from"), v.q.Get("move_to")
	if from == "" {
		return nil
	}
	if to == "" {
		t.Move = &moveView{MoveOffer: &ask.MoveOffer{Line: "Choose who should get the work."}}
		return nil
	}
	o, err := ask.OfferMove(v.ctx, v.tx, v.c.session.PersonID, from, to)
	if err != nil {
		return err
	}
	t.Move = newMoveView(o, v.q.Get("pick") == "1")
	return nil
}

// keepMove puts back, on the list shown again, the note typed and (with ticked) the boxes as they were
// ticked, so nothing has to be typed or ticked again.
func keepMove(note string, ticked []string) func(sc *screen) {
	return func(sc *screen) {
		if sc.Tell == nil || sc.Tell.Move == nil {
			return
		}
		mv := sc.Tell.Move
		mv.Note = note
		if ticked != nil && mv.Pick {
			for i := range mv.Shown {
				mv.Shown[i].Ticked = slices.Contains(ticked, mv.Shown[i].ID)
			}
		}
	}
}

// move moves the ticked items of someone's open work to another person, as the admin pressing. A
// refused press shows the list again, with the boxes ticked and the note typed, under the reason. The
// Pick button posts here too (show=pick): it shows the list with a box for every item, and moves
// nothing.
func (s *Server) move(w http.ResponseWriter, r *http.Request, c *call) {
	if err := r.ParseForm(); err != nil {
		s.problem(w, r, c, "", errors.New("that form couldn't be read; please try again"))
		return
	}
	ids := r.PostForm["item"]
	from, to, note := r.PostFormValue("move_from"), r.PostFormValue("move_to"), r.PostFormValue("note")
	pick := r.PostFormValue("pick") == "1"
	// again is the list's request with only who from and to (and Pick) in its query.
	again := func(pick bool) *http.Request {
		q := url.Values{"view": {"tell"}, "move_from": {from}, "move_to": {to}}
		if pick {
			q["pick"] = []string{"1"}
		}
		a := r.Clone(r.Context())
		a.URL.RawQuery = q.Encode()
		return a
	}
	if r.PostFormValue("show") == "pick" {
		s.show(w, again(true), c, "", http.StatusOK, "", "", keepMove(note, nil))
		return
	}
	refuse := func(err error) {
		ticked := []string(nil)
		if pick {
			ticked = append([]string{}, ids...)
		}
		s.problem(w, again(pick), c, "", err, keepMove(note, ticked))
	}
	if len(ids) == 0 {
		refuse(errors.New("tick at least one item to move"))
		return
	}
	var done workitem.Reassigned
	var toName string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		var err error
		if done, err = workitem.Reassign(r.Context(), tx, ids, from, to, note,
			ledger.PersonActor(c.session.PersonID)); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			to).Scan(&toName)
	})
	if err != nil {
		refuse(err)
		return
	}
	s.show(w, r, c, "", http.StatusOK, "", movedLine(done, toName))
}

// movedLine says what a press did: "Moved 3 items to Dev.", then any item that didn't move and why,
// and the notices an admin's Pass on gives.
func movedLine(done workitem.Reassigned, to string) string {
	var lines []string
	switch n := len(done.Moved); n {
	case 0:
		lines = append(lines, "Nothing was moved.")
	case 1:
		lines = append(lines, "Moved 1 item to "+to+".")
	default:
		lines = append(lines, fmt.Sprintf("Moved %d items to %s.", n, to))
	}
	for _, sk := range done.Skipped {
		lines = append(lines, sk.Number+" was not moved: "+strings.TrimSuffix(sk.Why, ".")+".")
	}
	return strings.Join(append(lines, done.Notices...), "\n")
}

// stranded is the admins' card hint for an item whose holder can't work it any more (D75, M3): "With
// Ben, who is no longer active: pass it on", or "With Ben, who is no longer in Operations: pass it on".
// It is "" otherwise.
func (v *view) stranded(p *playbook.Playbook, it workitem.Item, members []workitem.Member) (string, error) {
	if v.lost == nil {
		lost, err := workitem.FormerRoles(v.ctx, v.tx)
		if err != nil {
			return "", err
		}
		v.lost = lost
	}
	gone, role := workitem.Stranded(p, it, members, v.lost)
	name := v.names[it.AssigneeID]
	switch {
	case gone:
		return "With " + name + ", who is no longer active: pass it on", nil
	case role != "":
		return "With " + name + ", who is no longer in " + v.roleName(role) + ": pass it on", nil
	}
	return "", nil
}

// roleName is a role's name, or its key when it has none.
func (v *view) roleName(key string) string {
	if n := v.roles[key]; n != "" {
		return n
	}
	return key
}

// holding is how many open items a person has, and how many of them in a step they can no longer work.
type holding struct{ open, stuck int }

// holdings counts, per person, the open items assigned to them (D75, M3), for Settings → People.
func (v *view) holdings() (map[string]holding, error) {
	items, err := workitem.List(v.ctx, v.tx)
	if err != nil {
		return nil, err
	}
	members, err := v.members()
	if err != nil {
		return nil, err
	}
	if v.lost == nil {
		if v.lost, err = workitem.FormerRoles(v.ctx, v.tx); err != nil {
			return nil, err
		}
	}
	out := map[string]holding{}
	for _, it := range items {
		if it.AssigneeID == "" {
			continue
		}
		p, err := v.playbook(it)
		if err != nil {
			return nil, err
		}
		h := out[it.AssigneeID]
		h.open++
		if _, role := workitem.Stranded(p, it, members, v.lost); role != "" {
			h.stuck++
		}
		out[it.AssigneeID] = h
	}
	return out, nil
}

// items says a number of open items: "1 open item", "3 open items".
func items(n int) string {
	if n == 1 {
		return "1 open item"
	}
	return fmt.Sprintf("%d open items", n)
}
