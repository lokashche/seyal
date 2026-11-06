// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seyal/internal/ai"
	"seyal/internal/ask"
	"seyal/internal/auth"
	"seyal/internal/channel"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/thread"
	"seyal/internal/workitem"
)

// screen is what the three-pane page shows. Item is nil on the Tell Seyal view.
type screen struct {
	Work     *myWork
	Current  string // id of the open item
	Tell     *tellView
	Item     *itemView
	TellView bool // phones show Tell Seyal on its own, not the list (?view=tell, or after sending)
}

type row struct {
	ID, Number, Title, Kind, Pill, Status string
	Hint                                  string // what happens next, for the person looking
	Preview                               string // the start of the latest message
	Ago                                   string // how long since it last changed
	Owner, OwnerName                      string // initials and name of who has it
	Mine                                  bool   // the person (or their team) should act
	Take                                  string // the holder the card shows (workitem.Item.HolderKey), when it offers "Take it"
	Quoted                                bool   // Title is the start of the first message (the item has no title yet)
}

type tellView struct {
	Processes   []tellProcess // shown in the right pane; "let Seyal decide" is chosen until the person picks one
	Placeholder string        // an example message from the processes
	Pending     *pendingView  // a saved message waiting for the person to confirm its process
	Answer      *ask.Answer   // Seyal's answer to a question about the work (D67)
	StartAs     []tellProcess // with an answer Seyal couldn't give: the processes to start the message as instead
	CanAsk      bool          // AI is on, so people may ask about the work here
}

// pendingView asks the person to confirm the process Seyal suggests for their message.
type pendingView struct {
	MessageID, Text string
	Files           int          // attached files, not counting the recording the words were spoken in
	Spoken          string       // that recording's address, when the words were spoken
	Suggested       *tellProcess // nil when Seyal isn't sure
	Others          []tellProcess
}

// tellProcess is one process people can start on Tell Seyal: chosen in the right pane, searchable.
type tellProcess struct {
	ID, Name, Description, Example string
	Search                         string // name, description and examples, for the search box
	Fallback                       bool   // for work that fits no other process
}

type itemView struct {
	Item    workitem.Item
	Kind    string
	Pill    string
	Status  string
	With    string // who has it now: a person, or a team
	Since   string // since when they have it: "today" or a day like "3 Nov"; "" for items from before D70
	Heading string // the title, or the start of the first message while it has none
	Links   []linkView
	Closed  bool
	NotMine bool // open, but this person may only read it and write notes (D55)
	// PassOnly names the step's team when the person has the item but isn't in that team, so they may
	// only pass it on (D68, choice 22).
	PassOnly string
	Hand     *handView // nil when the item is closed
	Thread   []entryView
	Quick    []ask.Quick // buttons that send a reply: an answer to Seyal's question, or a "which one?" choice
	Fields   []fieldView
	Slots    []slotView
	Actions  []actionView
}

type entryView struct {
	Kind   string // "me", "bot", "other" or "evt"
	Who    string
	Text   string
	Time   string
	Media  []mediaView
	Spoken bool // the text came from a voice note in Media (the one marked Spoken)
	Tidied bool // the AI tidy-up changed the words heard (removed "umm"s, repeats)
	Edited bool // the person corrected the words they were given
}

// linkView is a linked work item: the one this item is part of, or one it started.
type linkView struct {
	ID, Number, Title, Kind, Status, Relation string
}

// handView is the side panel's "who has it" part (D68, M1 and M2): Pass on, Take it, Give back to
// the team. Each form sends Holder back, so a form shown before the item changed hands does nothing.
type handView struct {
	Holder   string           // workitem.Item.HolderKey
	CanPass  bool             // whoever has it, or an admin
	To       []receiverOption // who may receive it in this step
	Others   []receiverOption // for admins: every other active person (passing to them shows a warning)
	GiveBack *receiverOption  // the step's team, when the person took it from there
	CanTake  bool             // it is with the person's team, or with nobody and they may take it
	Nobody   bool             // for admins: nobody can take it, so an admin should pass it on
	MaxNote  int              // the longest note, in letters
	// Draft is what the person sent with a Pass on that was refused, so the form comes back open with
	// their choice and note in it, and they needn't type the note again.
	Draft *passDraft
}

type passDraft struct{ To, Note string }

// Chosen reports whether key is the receiver a refused Pass on had chosen.
func (h *handView) Chosen(key string) bool { return h.Draft != nil && h.Draft.To == key }

type receiverOption struct {
	Key, Name string // Key is "person:<id>" or "team:<role>"
	Empty     bool   // a team nobody is in yet
}

type mediaView struct {
	URL          string
	Audio, Image bool
	Spoken       bool // the voice note the message's words came from: a small play button, not a player
}

type fieldView struct {
	Key, Label, Value string
	Missing           bool   // Seyal asks for it in this step
	From              string // e.g. "Nature of work" when the value is still what it pre-filled
	Editable          bool
	Input             string // "text", "date", "lines", "select" or "multi"
	Raw               string // current value as the input shows it
	Options           []option
}

type slotView struct {
	Slot, Name, Why, By string
	Done, CanDecide     bool
}

type actionView struct {
	Name, Label string
	NeedsReason bool
	Hint        string // where the item goes, when worth saying (workitem.Landing)
}

// view is everything read for one page, inside one transaction.
type view struct {
	ctx       context.Context
	tx        pgx.Tx
	c         *call
	today     string
	names     map[string]string // person id → name
	roles     map[string]string // role key → name
	masters   map[string]string // "kind/key" → name
	playbooks map[string]*playbook.Playbook
	q         url.Values         // the page's query: which folder, process and order My Work shows
	active    *[]workitem.Member // active people with a login, once read (see members)
}

func (s *Server) newView(r *http.Request, tx pgx.Tx, c *call) (*view, error) {
	v := &view{ctx: r.Context(), tx: tx, c: c, today: time.Now().In(c.tenant.Zone).Format(time.DateOnly),
		playbooks: map[string]*playbook.Playbook{}, q: r.URL.Query()}
	var err error
	if v.names, err = pairs(v, "SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant()"); err != nil {
		return nil, err
	}
	if v.roles, err = pairs(v, "SELECT key, name FROM roles WHERE tenant_id = current_tenant()"); err != nil {
		return nil, err
	}
	v.masters, err = pairs(v, "SELECT kind || '/' || key, name FROM master_items WHERE tenant_id = current_tenant()")
	return v, err
}

func pairs(v *view, sql string) (map[string]string, error) {
	rows, _ := v.tx.Query(v.ctx, sql)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ K, V string }])
	out := make(map[string]string, len(list))
	for _, p := range list {
		out[p.K] = p.V
	}
	return out, err
}

func (v *view) playbook(it workitem.Item) (*playbook.Playbook, error) {
	if p := v.playbooks[it.PlaybookVersionID]; p != nil {
		return p, nil
	}
	p, err := workitem.PlaybookFor(v.ctx, v.tx, it)
	v.playbooks[it.PlaybookVersionID] = p
	return p, err
}

// groups builds the My Work list. Each open item appears once, in the first group it fits:
// waiting for my approval, overdue, assigned to me, my drafts, other open work.
// mine reports whether the item waits for a decision the person can make.
func (v *view) mine(it workitem.Item) bool {
	st, err := workitem.Approvals(v.ctx, v.tx, it.ID)
	if err != nil {
		return false
	}
	return canDecide(st, v.c.session) != nil
}

// canDecide returns the slots the person may decide now: waiting, one of their roles, and they
// haven't decided another slot of this version (one decision per person per version).
func canDecide(st engine.ApprovalStatus, s auth.Session) map[string]bool {
	for _, slot := range st.Slots {
		if slot.ApprovedBy == s.PersonID {
			return nil
		}
	}
	var ok map[string]bool
	for _, slot := range st.Slots {
		if slices.Contains(st.Waiting, slot.Slot) && slices.Contains(s.Roles, slot.Role) {
			if ok == nil {
				ok = map[string]bool{}
			}
			ok[slot.Slot] = true
		}
	}
	return ok
}

func (v *view) status(p *playbook.Playbook, it workitem.Item) (pill, status string) {
	status = human(it.State)
	switch {
	case engine.IsClosed(p, it.State):
		return "p-closed", status
	case it.DueOn != "" && it.DueOn < v.today:
		return "p-late", status + " · overdue"
	case p.Approvals != nil && it.State == p.Approvals.OnComplete:
		return "p-ok", status
	case it.State == p.Initial:
		return "p-draft", status
	}
	return "p-wait", status
}

// current lists the version new work starts on, for each process the business has switched on.
func (v *view) current() ([]struct{ ID, Source string }, error) {
	rows, _ := v.tx.Query(v.ctx, `
		SELECT pv.id::text, pv.source
		FROM tenant_playbooks tp
		JOIN playbook_versions pv ON pv.tenant_id = tp.tenant_id AND pv.id = tp.current_version_id
		WHERE tp.tenant_id = current_tenant() AND tp.active
		ORDER BY tp.playbook_id`)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, Source string }])
}

// tellView offers the current version of each process the business has switched on.
func (v *view) tell() (*tellView, error) {
	list, err := v.current()
	if err != nil || len(list) == 0 {
		return nil, err
	}
	t := &tellView{}
	for _, pv := range list {
		p, err := playbook.Parse([]byte(pv.Source))
		if err != nil {
			return nil, err
		}
		tp := tellProcess{ID: pv.ID, Name: sentence(p.ID), Description: p.Description, Fallback: p.Fallback,
			Search: strings.ToLower(strings.Join(append([]string{sentence(p.ID), p.Description}, p.Examples...), " "))}
		if len(p.Examples) > 0 {
			tp.Example = p.Examples[0]
			if t.Placeholder == "" && !p.Fallback {
				t.Placeholder = "For example: " + p.Examples[0]
			}
		}
		t.Processes = append(t.Processes, tp)
	}
	// The catch-all comes last: people look for a real process first.
	slices.SortStableFunc(t.Processes, func(a, b tellProcess) int {
		switch {
		case a.Fallback == b.Fallback:
			return 0
		case a.Fallback:
			return 1
		}
		return -1
	})
	if t.Placeholder == "" {
		t.Placeholder = "Write the work as you would tell a colleague"
	}
	return t, nil
}

func (v *view) item(id string) (*itemView, error) {
	it, err := workitem.Get(v.ctx, v.tx, id)
	if err != nil {
		return nil, err
	}
	p, err := v.playbook(it)
	if err != nil {
		return nil, err
	}
	iv := &itemView{Item: it, Kind: sentence(p.ID), Closed: engine.IsClosed(p, it.State)}
	iv.Pill, iv.Status = v.status(p, it)
	switch {
	case it.AssigneeID != "":
		iv.With = v.show(playbook.Field{Type: "person"}, it.AssigneeID)
	case it.Team != "":
		var name string
		if err := v.tx.QueryRow(v.ctx, `SELECT name FROM roles WHERE tenant_id = current_tenant() AND key = $1`,
			it.Team).Scan(&name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		iv.With = cmp.Or(name, it.Team) + " team"
	}
	iv.Since = v.since(it.HeldSince)

	if iv.Links, err = v.links(it.ID); err != nil {
		return nil, err
	}

	entries, err := thread.For(v.ctx, v.tx, id)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		ev := entryView{Kind: "evt", Who: e.Who, Text: e.Text, Time: v.when(e.At)}
		if m := e.Message; m != nil {
			switch {
			case m.AuthorKind == "seyal":
				ev.Kind = "bot"
			case m.PersonID == v.c.session.PersonID:
				ev.Kind = "me"
			default:
				ev.Kind = "other"
			}
			if t := m.Transcript; t != nil && m.Text != "" {
				ev.Spoken, ev.Tidied, ev.Edited = true, t.Tidied != "" && t.Tidied != t.Raw, t.Edited
			}
			for _, md := range m.Media {
				audio := strings.HasPrefix(md.ContentType, "audio/")
				ev.Media = append(ev.Media, mediaView{URL: "/t/" + v.c.tenant.Slug + "/media/" + md.ID,
					Audio: audio, Image: strings.HasPrefix(md.ContentType, "image/"),
					Spoken: audio && ev.Spoken && md.ID == m.Transcript.MediaID})
			}
		}
		iv.Thread = append(iv.Thread, ev)
	}
	iv.Heading = it.Title
	if untitled(it.Title) {
		iv.Heading = "Untitled"
		for _, e := range iv.Thread {
			if e.Kind != "evt" && e.Kind != "bot" && strings.TrimSpace(e.Text) != "" {
				iv.Heading = "“" + strings.Join(strings.Fields(e.Text), " ") + "”"
				break
			}
		}
	}

	if !iv.Closed {
		switch err := workitem.MayChange(v.ctx, v.tx, it, ledger.PersonActor(v.c.session.PersonID)); {
		case errors.Is(err, workitem.ErrNotYours):
			iv.NotMine = true
		case err != nil:
			return nil, err
		}
	}
	changeable := !iv.Closed && !iv.NotMine
	if !iv.Closed {
		if iv.Hand, err = v.hand(p, it); err != nil {
			return nil, err
		}
		iv.PassOnly = v.passOnly(p, it)
	}

	toAsk := engine.ToAsk(p, it.State, it.Fields) // what Seyal asks in this step
	from, err := v.prefilled(p, it)
	if err != nil {
		return nil, err
	}
	kind, owner := engine.Owner(p, it.State)
	power := engine.PowerFields(p)
	for _, f := range p.Fields {
		// A person field that says who gets the item or who may take a move is changed only by whoever
		// has the item or an admin; the one for this step hands the item over (D68, choice 14).
		open := changeable && (!power[f.Key] || (iv.Hand != nil && iv.Hand.CanPass))
		fv, err := v.field(f, it.Fields[f.Key], open)
		if err != nil {
			return nil, err
		}
		if fv.Editable && kind == "field" && f.Key == owner {
			fv.Options = receiverChoices(iv.Hand, it.AssigneeID, it.Fields[f.Key], fv.Options)
		}
		fv.Missing = !iv.Closed && slices.Contains(toAsk, f.Key)
		fv.From = from[f.Key]
		iv.Fields = append(iv.Fields, fv)
	}

	if changeable {
		f, offer, err := ask.Pending(v.ctx, v.tx, it)
		switch {
		case err != nil:
			return nil, err
		case offer != "":
			iv.Quick = ask.Replies("yes", "no") // "add it as a new entry?"
		case f != nil:
			iv.Quick = ask.Replies(v.quick(*f)...)
		}
	}
	if !iv.Closed {
		// Seyal's Yes and No for a suggestion (D74), or its "which one do you mean?" after a typed hand-over
		// (D73), wins: only the person asked sees the buttons, and every check runs again when one is
		// pressed.
		choose, err := ask.Choosing(v.ctx, v.tx, it, v.c.session.PersonID)
		if err != nil {
			return nil, err
		}
		if choose != nil {
			iv.Quick = choose
		}
	}
	if !iv.Closed {
		// Only the moves this person may take (D68): a process can say per move who takes it, so
		// someone who can't change the details may still have a move, and the other way round.
		for _, name := range engine.ActionsFor(p, it.Engine(), v.actor()) {
			a := actionView{Name: name, Label: sentence(name)}
			for _, t := range p.Transitions {
				if t.Action == name && t.Needs == "reason" {
					a.NeedsReason = true
				}
			}
			// "Goes back to Asha.", or a warning before the move (D68, choices 7 and 13).
			if a.Hint, err = workitem.Landing(v.ctx, v.tx, it, name, v.actor()); err != nil {
				return nil, err
			}
			iv.Actions = append(iv.Actions, a)
		}
	}

	if p.Approvals != nil {
		st, err := workitem.Approvals(v.ctx, v.tx, id)
		if err != nil {
			return nil, err
		}
		allowed := map[string]bool{}
		if it.State == p.Approvals.In {
			allowed = canDecide(st, v.c.session)
		}
		for _, slot := range st.Slots {
			if !slot.Required {
				continue
			}
			sv := slotView{Slot: slot.Slot, Name: v.roles[slot.Role], Why: slot.Why, CanDecide: allowed[slot.Slot]}
			if sv.Name == "" {
				sv.Name = sentence(slot.Slot)
			}
			if slot.ApprovedBy != "" {
				sv.Done, sv.By = true, v.names[slot.ApprovedBy]
			}
			iv.Slots = append(iv.Slots, sv)
		}
	}
	return iv, nil
}

// quick offers one-tap answers for the question Seyal is waiting on, when there are few choices.
func (v *view) quick(f playbook.Field) []string {
	switch f.Type {
	case "choice":
		return f.Options
	case "bool":
		return []string{"yes", "no"}
	case "master":
		// Only entries still in use: a retired master item isn't offered for new work.
		rows, _ := v.tx.Query(v.ctx, `
			SELECT name FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND active
			ORDER BY name LIMIT 9`, f.Kind)
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err == nil && len(names) <= 8 {
			return names
		}
	}
	return nil
}

// since shows a hand-over's moment (workitem.HeldSinceLayout) as a day: "today", "3 Nov", or
// "3 Nov 2025" in another year. It is "" when the item has no such moment.
func (v *view) since(held string) string {
	t, err := time.Parse(workitem.HeldSinceLayout, held)
	if err != nil {
		return ""
	}
	t = t.In(v.c.tenant.Zone)
	switch {
	case t.Format(time.DateOnly) == v.today:
		return "today"
	case strings.HasPrefix(v.today, t.Format("2006")):
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

func (v *view) when(t time.Time) string {
	t = t.In(v.c.tenant.Zone)
	if t.Format(time.DateOnly) == v.today {
		return t.Format("15:04")
	}
	return t.Format("2 Jan, 15:04")
}

// --- handlers ---

func (s *Server) tellPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showScreen(w, r, c, "", http.StatusOK, "")
}

func (s *Server) itemPage(w http.ResponseWriter, r *http.Request, c *call) {
	s.showScreen(w, r, c, r.PathValue("id"), http.StatusOK, "")
}

// showScreen renders the three panes, with an item open or not, and an optional problem to show.
func (s *Server) showScreen(w http.ResponseWriter, r *http.Request, c *call, itemID string, status int, problem string,
	more ...func(sc *screen)) {
	s.show(w, r, c, itemID, status, problem, "", more...)
}

// show is showScreen with an optional notice too, e.g. Seyal's reply to a greeting on Tell Seyal.
// more, if given, adds to the screen inside the same transaction (e.g. a process to confirm, or a
// refused form's draft).
func (s *Server) show(w http.ResponseWriter, r *http.Request, c *call, itemID string, status int, problem, notice string,
	more ...func(sc *screen)) {
	var sc screen
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		v, err := s.newView(r, tx, c)
		if err != nil {
			return err
		}
		if sc.Work, err = v.myWork(); err != nil {
			return err
		}
		if itemID == "" {
			sc.TellView = r.Method == http.MethodPost || r.URL.Query().Get("view") == "tell"
			sc.Tell, err = v.tell()
			if sc.Tell != nil {
				_, sc.Tell.CanAsk = s.ai.For(ai.Understand)
			}
			for _, m := range more {
				m(&sc)
			}
			return err
		}
		sc.Current = itemID
		if sc.Item, err = v.item(itemID); err != nil {
			return err
		}
		for _, m := range more {
			m(&sc)
		}
		return nil
	})
	if errors.Is(err, workitem.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := c.page(sc)
	p.Error, p.Notice = problem, notice
	s.render(w, r, status, "app", p)
}

// tell starts work from Tell Seyal. With one process, or one the person picked, it starts at once.
// With "let Seyal decide", the message is saved, Seyal suggests a process, and the person confirms
// it (a second post with the saved message's id).
func (s *Server) tell(w http.ResponseWriter, r *http.Request, c *call) {
	started := time.Now() // the request's time limit runs from about now (see answer)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") && r.PostFormValue("message") != "" {
		s.confirmTell(w, r, c)
		return
	}
	in, err := s.incoming(w, r, c)
	if err == nil && in.Text == "" && len(in.Files) == 0 && in.Spoken == nil {
		err = errors.New("please write or record what the work is")
	}
	if err != nil {
		voiceRefused(w, err)
		s.showScreen(w, r, c, "", http.StatusUnprocessableEntity, capital(err.Error())+".")
		return
	}
	var reply ask.Reply
	var routing *ask.Routing
	picked := r.FormValue("playbook") // the process the person chose, or "auto"
	err = db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		// New work may only start on the current version of a process that is switched on.
		current, err := (&view{ctx: r.Context(), tx: tx}).current()
		if err != nil {
			return err
		}
		chosen := r.FormValue("playbook")
		if (chosen == "" || chosen == "auto") && len(current) > 1 {
			processes := make([]ask.Process, 0, len(current))
			for _, pv := range current {
				p, err := playbook.Parse([]byte(pv.Source))
				if err != nil {
					return err
				}
				processes = append(processes, ask.Process{VersionID: pv.ID, ID: p.ID, Name: sentence(p.ID),
					Description: p.Description, Examples: p.Examples, Fallback: p.Fallback})
			}
			rt, err := ask.Route(r.Context(), tx, s.env(), processes, in)
			routing = &rt
			return err
		}
		if chosen == "" || chosen == "auto" {
			if len(current) == 0 {
				return errors.New("no process is switched on yet")
			}
			chosen = current[0].ID
		}
		if !slices.ContainsFunc(current, func(p struct{ ID, Source string }) bool { return p.ID == chosen }) {
			return errors.New("that process isn't in use any more; reload the page")
		}
		reply, err = ask.Begin(r.Context(), tx, s.env(), chosen, in)
		return err
	})
	if err != nil {
		s.problem(w, r, c, "", err)
		return
	}
	// A question about the work is answered now that the message is saved, in a transaction of its
	// own: its AI call and search then keep nobody else in the business waiting (D67).
	asked := reply.Asked
	if routing != nil {
		asked = routing.Asked
	}
	if asked != nil {
		a, said, err := s.answer(r, c, *asked, started)
		switch {
		case err != nil:
			s.problem(w, r, c, "", err)
		case a != nil:
			s.showAnswer(w, r, c, a, picked)
		default:
			s.show(w, r, c, "", http.StatusOK, "", texts(said))
		}
		return
	}
	if routing != nil {
		s.askWhichProcess(w, r, c, *routing)
		return
	}
	if reply.Answer != nil {
		s.showAnswer(w, r, c, reply.Answer, picked)
		return
	}
	if reply.Item.ID == "" { // a greeting or a question: nothing started, Seyal just answers
		s.show(w, r, c, "", http.StatusOK, "", texts(reply.Seyal))
		return
	}
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/items/"+reply.Item.ID, http.StatusSeeOther)
}

// askWhichProcess shows Seyal's suggestion for a saved message, or its reply to a greeting.
func (s *Server) askWhichProcess(w http.ResponseWriter, r *http.Request, c *call, rt ask.Routing) {
	if rt.Answer != nil {
		s.showAnswer(w, r, c, rt.Answer, "")
		return
	}
	if len(rt.Seyal) > 0 {
		s.show(w, r, c, "", http.StatusOK, "", texts(rt.Seyal))
		return
	}
	s.show(w, r, c, "", http.StatusOK, "", "", func(sc *screen) {
		if sc.Tell == nil {
			return
		}
		m := rt.Message
		pv := &pendingView{MessageID: m.ID, Text: m.Text}
		for _, md := range m.Media {
			// The recording the words were spoken in isn't an attached file (as in the thread).
			if t := m.Transcript; t != nil && m.Text != "" && md.ID == t.MediaID {
				pv.Spoken = "/t/" + c.tenant.Slug + "/media/" + md.ID
				continue
			}
			pv.Files++
		}
		for i := range sc.Tell.Processes {
			if p := sc.Tell.Processes[i]; p.ID == rt.Suggested {
				pv.Suggested = &p
			} else {
				pv.Others = append(pv.Others, p)
			}
		}
		sc.Tell.Pending = pv
	})
}

// texts joins what Seyal said, one message a line.
func texts(said []channel.Message) string {
	lines := make([]string, len(said))
	for i, m := range said {
		lines[i] = m.Text
	}
	return strings.Join(lines, "\n")
}

// questionsPerMinute is how many questions about the work one login may ask in a minute. Each costs
// a second AI call; over the limit, Seyal says so and doesn't call the model.
const questionsPerMinute = 10

// answerRoom is the part of a request's time limit that answering a question leaves unused: up to
// 10 seconds of reading the request's headers before started, then committing and writing the page.
const answerRoom = 20 * time.Second

// answer answers a question that Tell Seyal saved, in a new transaction (see ask.Asked). The limit
// counts the login, which may be used in several businesses (D64). started is when the request
// began: the answer must be ready before the web server's time limit for it runs out, so it gets a
// deadline, and with too little time left (a slow first AI call) ask skips the search (D67).
func (s *Server) answer(r *http.Request, c *call, q ask.Asked, started time.Time) (*ask.Answer, []channel.Message, error) {
	login := c.session.AccountID
	if login == "" {
		login = c.tenant.ID + "/" + c.session.PersonID
	}
	if !s.askRate.take(login) {
		return ask.Unread(s.env(), q.MessageID, q.Question, ask.Busy), nil, nil
	}
	ctx, cancel := context.WithDeadline(r.Context(), started.Add(s.limit-answerRoom))
	defer cancel()
	if ctx.Err() != nil { // no time left even to look: say so, rather than show an error
		return ask.Unread(s.env(), q.MessageID, q.Question, ""), nil, nil
	}
	var a *ask.Answer
	var said []channel.Message
	err := db.WithTenant(ctx, s.app, c.tenant.ID, func(tx pgx.Tx) error {
		var err error
		a, said, err = ask.AnswerAsked(ctx, tx, s.env(), q)
		return err
	})
	return a, said, err
}

// showAnswer shows Seyal's answer to a question about the work on Tell Seyal, with links to the
// items it found (D67). It is shown once, not kept. When Seyal couldn't read the message, it offers
// to start it as work: as the process the person picked, or else as any of them.
func (s *Server) showAnswer(w http.ResponseWriter, r *http.Request, c *call, a *ask.Answer, picked string) {
	s.show(w, r, c, "", http.StatusOK, "", "", func(sc *screen) {
		if sc.Tell == nil {
			return
		}
		sc.Tell.Answer = a
		if a.MessageID == "" {
			return
		}
		for _, p := range sc.Tell.Processes {
			if p.ID == picked {
				sc.Tell.StartAs = []tellProcess{p}
				return
			}
		}
		sc.Tell.StartAs = sc.Tell.Processes
	})
}

// confirmTell starts the process the person confirmed for a message saved earlier.
func (s *Server) confirmTell(w http.ResponseWriter, r *http.Request, c *call) {
	var reply ask.Reply
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		current, err := (&view{ctx: r.Context(), tx: tx}).current()
		if err != nil {
			return err
		}
		chosen := r.PostFormValue("playbook")
		if !slices.ContainsFunc(current, func(p struct{ ID, Source string }) bool { return p.ID == chosen }) {
			return errors.New("that process isn't in use any more; reload the page")
		}
		reply, err = ask.BeginFrom(r.Context(), tx, s.env(), chosen, r.PostFormValue("message"), c.session.PersonID)
		return err
	})
	if err != nil {
		s.problem(w, r, c, "", err)
		return
	}
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/items/"+reply.Item.ID, http.StatusSeeOther)
}

func (s *Server) say(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	in, err := s.incoming(w, r, c)
	if err == nil && in.Text == "" && len(in.Files) == 0 && in.Spoken == nil {
		err = errors.New("please write a message, record a voice note or attach a file")
	}
	if err != nil {
		voiceRefused(w, err)
		s.showScreen(w, r, c, id, http.StatusUnprocessableEntity, capital(err.Error())+".")
		return
	}
	err = db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		_, err := ask.Converse(r.Context(), tx, s.env(), id, in)
		return err
	})
	s.done(w, r, c, id, err)
}

// env is what the chat needs besides the database.
func (s *Server) env() ask.Env {
	return ask.Env{Store: s.store, AI: s.ai}
}

// maxFiles is how many files one message may carry.
const maxFiles = 5

// uploads limits how many messages with files are read at the same time (see incoming).
var uploads = make(chan struct{}, 4)

// incoming reads the composer form: the text, plus a recorded voice note and attached files when the
// form is sent as multipart. A voice note sent with a voice token (voice_token, see voice.go) is one
// whose words the person checked: it becomes the message's Spoken recording, with what was heard.
// The whole request is limited in size before anything is read.
func (s *Server) incoming(w http.ResponseWriter, r *http.Request, c *call) (channel.Incoming, error) {
	in := channel.Incoming{Channel: channel.Web{}.Name(), PersonID: c.session.PersonID}
	r.Body = http.MaxBytesReader(w, r.Body, maxFiles*media.MaxSize+1<<20)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		// Files are held in memory while they are checked and stored, so only a few uploads are
		// handled at once; the others wait their turn (or give up when the person leaves).
		select {
		case uploads <- struct{}{}:
			defer func() { <-uploads }()
		case <-r.Context().Done():
			return in, r.Context().Err()
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			return in, errors.New("the message or its files are too large")
		}
		token := strings.TrimSpace(r.FormValue("voice_token"))
		for _, field := range []string{"voice", "file"} {
			for _, fh := range r.MultipartForm.File[field] {
				if fh.Size == 0 {
					continue // an empty file input
				}
				count := len(in.Files)
				if in.Spoken != nil {
					count++
				}
				if count == maxFiles {
					return in, fmt.Errorf("a message can carry at most %d files", maxFiles)
				}
				if fh.Size > media.MaxSize {
					return in, fmt.Errorf("files can be at most %d MB", media.MaxSize>>20)
				}
				f, err := fh.Open()
				if err != nil {
					return in, err
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					return in, err
				}
				file := channel.File{ContentType: fh.Header.Get("Content-Type"), Data: data}
				if field == "voice" && token != "" && in.Spoken == nil {
					heard, err := s.heardFrom(token, c, time.Now())
					if err != nil {
						return in, err
					}
					in.Spoken = &channel.Spoken{File: file, Heard: heard}
					continue
				}
				in.Files = append(in.Files, file)
			}
		}
		if token != "" && in.Spoken == nil {
			return in, errHeard // the words came without their recording
		}
	}
	in.Text = strings.TrimSpace(strings.ReplaceAll(r.FormValue("text"), "\r\n", "\n")) // browsers send line breaks as CRLF
	// A reply button under the thread (a suggestion's Yes or No) says which suggestion it answers.
	in.Button = r.FormValue("button")
	return in, nil
}

// voiceRefused marks a refused message whose voice note's words Seyal can't vouch for (see
// heardFrom), so the composer keeps the recording and can offer to send it as a plain voice note.
func voiceRefused(w http.ResponseWriter, err error) {
	if errors.Is(err, errHeard) || errors.Is(err, channel.ErrVoiceChanged) {
		w.Header().Set("Seyal-Voice", "refused")
	}
}

func (s *Server) act(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		_, err := workitem.Act(r.Context(), tx, id, r.PostFormValue("action"), strings.TrimSpace(r.PostFormValue("reason")),
			ledger.PersonActor(c.session.PersonID))
		if err != nil {
			return err
		}
		return ask.Nudge(r.Context(), tx, s.env(), id)
	})
	s.done(w, r, c, id, err)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, c *call) {
	id := r.PathValue("id")
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		seen, _ := strconv.Atoi(r.PostFormValue("version")) // the version the page showed
		_, err := workitem.DecideSeen(r.Context(), tx, id, seen, c.session.PersonID, r.PostFormValue("slot"),
			r.PostFormValue("decision"), strings.TrimSpace(r.PostFormValue("reason")))
		if err != nil {
			return err
		}
		return ask.Nudge(r.Context(), tx, s.env(), id)
	})
	s.done(w, r, c, id, err)
}

// done sends the person back to the item after a change, or shows what was wrong.
func (s *Server) done(w http.ResponseWriter, r *http.Request, c *call, id string, err error) {
	if err != nil {
		s.problem(w, r, c, id, err)
		return
	}
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/items/"+id, http.StatusSeeOther)
}

// problem shows a refusal (missing fields, not allowed, ...) in plain words. Database errors are
// logged and shown only as a general message.
func (s *Server) problem(w http.ResponseWriter, r *http.Request, c *call, itemID string, err error, more ...func(sc *screen)) {
	if errors.Is(err, workitem.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		s.fail(w, r, err)
		return
	}
	voiceRefused(w, err)
	msg := err.Error()
	var missing *engine.MissingError
	if errors.As(err, &missing) {
		msg = "Still needed: " + strings.Join(s.labels(r, c, itemID, missing.Keys), ", ") + "."
	}
	s.showScreen(w, r, c, itemID, http.StatusUnprocessableEntity, capital(msg), more...)
}

func (s *Server) labels(r *http.Request, c *call, itemID string, keys []string) []string {
	out := slices.Clone(keys)
	db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		it, err := workitem.Get(r.Context(), tx, itemID)
		if err != nil {
			return err
		}
		p, err := workitem.PlaybookFor(r.Context(), tx, it)
		if err != nil {
			return err
		}
		for i, k := range keys {
			for _, f := range p.Fields {
				if f.Key == k {
					out[i] = ask.Label(f)
				}
			}
		}
		return nil
	})
	return out
}

// media sends a stored file, but only to people of the business that owns it.
func (s *Server) media(w http.ResponseWriter, r *http.Request, c *call) {
	var contentType, key string
	err := db.WithTenant(r.Context(), s.app, c.tenant.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT content_type, storage_key FROM media WHERE tenant_id = current_tenant() AND id::text = $1`,
			r.PathValue("id")).Scan(&contentType, &key)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, err := s.store.Open(key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer f.Close()
	// Serve only a plain, accepted type (files stored before types were cleaned may carry anything),
	// and never let a file act as a page of this site, even if a browser opens it directly.
	clean, ok := media.Clean(contentType)
	if !ok {
		clean = "application/octet-stream"
	}
	w.Header().Set("Content-Type", clean)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if !strings.HasPrefix(clean, "audio/") && !strings.HasPrefix(clean, "image/") {
		w.Header().Set("Content-Disposition", "attachment")
	}
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs) // supports seeking in voice notes
		return
	}
	io.Copy(w, f)
}

// human makes a key like "pending_approval" readable: "pending approval".
func human(s string) string { return strings.NewReplacer("_", " ", "-", " ").Replace(s) }

// sentence is human with a capital first letter: "leave-request" → "Leave request". Use it for keys,
// never for messages (it would hide the exact name of a misspelt key).
func sentence(s string) string {
	return capital(human(s))
}

// capital gives a message a capital first letter and changes nothing else.
func capital(s string) string {
	r, size := utf8.DecodeRuneInString(s) // the first letter, which may be more than one byte (e.g. Tamil)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
