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
	Work    *myWork
	Current string // id of the open item
	Tell    *tellView
	Item    *itemView
}

type row struct {
	ID, Number, Title, Kind, Pill, Status string
	Hint                                  string // what happens next, for the person looking
	Preview                               string // the start of the latest message
	Ago                                   string // how long since it last changed
	Owner, OwnerName                      string // initials and name of who has it
	Mine                                  bool   // the person (or their team) should act
	Quoted                                bool   // Title is the start of the first message (the item has no title yet)
}

type tellView struct {
	Processes   []tellProcess // shown in the right pane; "let Seyal decide" is chosen until the person picks one
	Placeholder string        // an example message from the processes
	Pending     *pendingView  // a saved message waiting for the person to confirm its process
}

// pendingView asks the person to confirm the process Seyal suggests for their message.
type pendingView struct {
	MessageID, Text string
	Files           int
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
	Heading string // the title, or the start of the first message while it has none
	Links   []linkView
	Closed  bool
	NotMine bool // open, but this person may only read it and write notes (D55)
	Thread  []entryView
	Quick   []string
	Fields  []fieldView
	Slots   []slotView
	Actions []actionView
}

type entryView struct {
	Kind  string // "me", "bot", "other" or "evt"
	Who   string
	Text  string
	Time  string
	Media []mediaView
}

// linkView is a linked work item: the one this item is part of, or one it started.
type linkView struct {
	ID, Number, Title, Kind, Status, Relation string
}

type mediaView struct {
	URL          string
	Audio, Image bool
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
	q         url.Values // the page's query: which folder, process and order My Work shows
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
			for _, md := range m.Media {
				ev.Media = append(ev.Media, mediaView{URL: "/t/" + v.c.tenant.Slug + "/media/" + md.ID,
					Audio: strings.HasPrefix(md.ContentType, "audio/"), Image: strings.HasPrefix(md.ContentType, "image/")})
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

	toAsk := engine.ToAsk(p, it.State, it.Fields) // what Seyal asks in this step
	from, err := v.prefilled(p, it)
	if err != nil {
		return nil, err
	}
	for _, f := range p.Fields {
		fv, err := v.field(f, it.Fields[f.Key], changeable)
		if err != nil {
			return nil, err
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
			iv.Quick = []string{"yes", "no"} // "add it as a new entry?"
		case f != nil:
			iv.Quick = v.quick(*f)
		}
		for _, name := range engine.Actions(p, engine.Item{State: it.State, Fields: it.Fields}) {
			a := actionView{Name: name, Label: sentence(name)}
			for _, t := range p.Transitions {
				if t.Action == name && t.Needs == "reason" {
					a.NeedsReason = true
				}
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
func (s *Server) showScreen(w http.ResponseWriter, r *http.Request, c *call, itemID string, status int, problem string) {
	s.show(w, r, c, itemID, status, problem, "")
}

// show is showScreen with an optional notice too, e.g. Seyal's reply to a greeting on Tell Seyal.
// more, if given, adds to the screen inside the same transaction (e.g. a process to confirm).
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
			sc.Tell, err = v.tell()
			for _, m := range more {
				m(&sc)
			}
			return err
		}
		sc.Current = itemID
		sc.Item, err = v.item(itemID)
		return err
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
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") && r.PostFormValue("message") != "" {
		s.confirmTell(w, r, c)
		return
	}
	in, err := s.incoming(w, r, c)
	if err == nil && in.Text == "" && len(in.Files) == 0 {
		err = errors.New("please write or record what the work is")
	}
	if err != nil {
		s.showScreen(w, r, c, "", http.StatusUnprocessableEntity, capital(err.Error())+".")
		return
	}
	var reply ask.Reply
	var routing *ask.Routing
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
	if routing != nil {
		s.askWhichProcess(w, r, c, *routing)
		return
	}
	if reply.Item.ID == "" { // a greeting or a question: nothing started, Seyal just answers
		var said []string
		for _, m := range reply.Seyal {
			said = append(said, m.Text)
		}
		s.show(w, r, c, "", http.StatusOK, "", strings.Join(said, "\n"))
		return
	}
	http.Redirect(w, r, "/t/"+c.tenant.Slug+"/items/"+reply.Item.ID, http.StatusSeeOther)
}

// askWhichProcess shows Seyal's suggestion for a saved message, or its reply to a greeting.
func (s *Server) askWhichProcess(w http.ResponseWriter, r *http.Request, c *call, rt ask.Routing) {
	if len(rt.Seyal) > 0 {
		var said []string
		for _, m := range rt.Seyal {
			said = append(said, m.Text)
		}
		s.show(w, r, c, "", http.StatusOK, "", strings.Join(said, "\n"))
		return
	}
	s.show(w, r, c, "", http.StatusOK, "", "", func(sc *screen) {
		if sc.Tell == nil {
			return
		}
		pv := &pendingView{MessageID: rt.Message.ID, Text: rt.Message.Text, Files: len(rt.Message.Media)}
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
	if err == nil && in.Text == "" && len(in.Files) == 0 {
		err = errors.New("please write a message, record a voice note or attach a file")
	}
	if err != nil {
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
// form is sent as multipart. The whole request is limited in size before anything is read.
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
		for _, field := range []string{"voice", "file"} {
			for _, fh := range r.MultipartForm.File[field] {
				if fh.Size == 0 {
					continue // an empty file input
				}
				if len(in.Files) == maxFiles {
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
				in.Files = append(in.Files, channel.File{ContentType: fh.Header.Get("Content-Type"), Data: data})
			}
		}
	}
	in.Text = strings.TrimSpace(r.FormValue("text"))
	return in, nil
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
func (s *Server) problem(w http.ResponseWriter, r *http.Request, c *call, itemID string, err error) {
	if errors.Is(err, workitem.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		s.fail(w, r, err)
		return
	}
	msg := err.Error()
	var missing *engine.MissingError
	if errors.As(err, &missing) {
		msg = "Still needed: " + strings.Join(s.labels(r, c, itemID, missing.Keys), ", ") + "."
	}
	s.showScreen(w, r, c, itemID, http.StatusUnprocessableEntity, capital(msg))
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
