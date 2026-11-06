// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/chase"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/hours"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// The phone-note reader (Phase 4, part 4a-4, D89). It is its own small loop next to the web server,
// not a chaser task: it runs even with SEYAL_CHASE_EVERY=off, and it never takes a business's ledger
// lock or the chaser's "one round at a time" lock. Each pass, for each business:
//
//  1. Read: the ledger events after the business's bookmark. Who (notify.go) says who gets a phone
//     note about each; each answer is one notices row. Made in quiet hours, it waits for the morning.
//     The rows and the moved bookmark are written in one transaction, so reading twice adds nothing.
//  2. Deliver: the notices that are due. Each is dropped if it no longer matters (the person opened
//     the item since, left, the item is closed, the approval was given, or it waited too long). What
//     is left becomes one phone note per person (grouped when there are several), within the hourly
//     cap, and is posted to the person's devices while its notices are locked (sender.go, part
//     4a-5). They are marked sent only when a device's push service took it, in that person's own
//     transaction, so a phone note is never lost, and a later failure never undoes the mark of one
//     already posted. A note no device took is tried again after 1, 5 and 30 minutes, then given up.
//     A note may go twice in two cases only: a crash just after a post, or a push service that took
//     it but answered too late (after 10 seconds), so it counts as not taken. The repeat has the same
//     tag, so the phone usually shows it in place of the first. Outside working hours Deliver sends
//     nothing: what waits joins the morning note.
//
// Besides the minute, a web request pokes the reader after a change (Poke), so a daytime hand-over
// buzzes within seconds. Phone notes are not ledger events (D85, decision 4): they only point at
// events that are.

const (
	Every      = time.Minute      // how often the reader runs
	MaxAge     = 12 * time.Hour   // an event older than this when the reader gets to it tells nobody
	TTL        = 12 * time.Hour   // how long a push service may keep a phone note
	MorningTTL = 8 * time.Hour    // the same, for the morning note
	MaxPerHour = 20               // phone notes per person per hour; more wait and go as one later
	pageSize   = 500              // ledger events read at a time
	maxPages   = 20               // pages per business per pass; the rest wait for the next pass
	businessIn = 30 * time.Second // for one business's Read, and again for its Deliver
	personIn   = 20 * time.Second // for one person's phone note: its checks, the hand-over and the mark
	sendIn     = 10 * time.Second // for posting one phone note to all of its devices (at once)
	markIn     = 5 * time.Second  // kept free after a hand-over, to save its mark
	logPrefix  = "phone notes"
)

// Note is one phone note for one person: what the lock screen shows and where a tap goes. It holds
// no device push address: Deliver looks those up, and the note is locked for each device on its way
// (sender.go), with the address the device used put in front of the path.
type Note struct {
	Tenant  string // the business's id
	Person  string // who is told
	Body    string // the lock screen's line, from Text, Several, Moved or Quiet; the title is always Title
	Path    string // where a tap goes, inside the business: /t/<slug>/items/<id>, or /t/<slug>/ (My Work)
	Tag     string // a short code made of letters and digits (never the item's number): Topic and tag
	TTL     time.Duration
	Urgency string  // "normal"
	Notices []int64 // the notices rows it carries
}

// Reader reads each business's new ledger events and makes its phone notes. Make one with New.
type Reader struct {
	DB  db.Beginner // the app pool: the app role, row-level security on
	Log *log.Logger
	// Push is the server's switch (SEYAL_PUSH=on). Off, the reader only moves each bookmark on.
	Push bool
	// Out posts phone notes to devices (sender.go): *push.Sender, or a fake in tests. nil: notices
	// are made and wait.
	Out  Pusher
	List func(ctx context.Context) ([]db.Tenant, error) // the businesses; nil: all of them
	Now  func() time.Time                               // the clock; nil: time.Now (tests use another)

	mu sync.Mutex // one pass at a time
	// keyWorks holds each push service (pushFamily, for example "googleapis.com") that took a phone
	// note since the server started: that push service accepts Seyal's key.
	keyWorks sync.Map

	pokeMu sync.Mutex
	poked  map[string]bool // the businesses poked since the last poked pass, by id
	pokes  chan struct{}   // one waiting poke at most: Poke never blocks
}

// New returns a reader for the app pool that writes its log lines to l. out posts the phone notes;
// nil means phone notes are off on this server (SEYAL_PUSH), and the reader only moves each
// business's bookmark on.
func New(pool db.Beginner, l *log.Logger, out Pusher) *Reader {
	return &Reader{DB: pool, Log: l, Push: out != nil, Out: out}
}

// PokeWait is how long the reader waits after a poke before it looks: the changes one person makes
// in a moment (a hand-over, then a note) go as one phone note, and a burst of pokes is one pass.
const PokeWait = 2 * time.Second

// Poke asks the reader to read and deliver for one business soon, without waiting for the minute: a
// web request calls it after a change, so a daytime hand-over buzzes within seconds. It never blocks,
// and it does nothing while phone notes are off on the server.
func (r *Reader) Poke(tenantID string) {
	if r == nil || !r.Push || r.Out == nil {
		return
	}
	r.pokeMu.Lock()
	if r.poked == nil {
		r.poked = map[string]bool{}
	}
	r.poked[tenantID] = true
	pokes := r.pokeChan()
	r.pokeMu.Unlock()
	select {
	case pokes <- struct{}{}:
	default: // a poke already waits: it covers this one too
	}
}

// pokeChan returns the poke channel, made on first use. r.pokeMu must be held.
func (r *Reader) pokeChan() chan struct{} {
	if r.pokes == nil {
		r.pokes = make(chan struct{}, 1)
	}
	return r.pokes
}

// Run runs a pass every `every` until ctx ends, and a pass for the poked businesses PokeWait after a
// poke. The first full pass runs `every` after start.
func (r *Reader) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	r.pokeMu.Lock()
	pokes := r.pokeChan()
	r.pokeMu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.Pass(ctx)
		case <-pokes:
			select {
			case <-ctx.Done():
				return
			case <-time.After(PokeWait):
			}
			r.pass(ctx, r.takePoked())
		}
	}
}

// takePoked returns, and forgets, the businesses poked so far.
func (r *Reader) takePoked() map[string]bool {
	r.pokeMu.Lock()
	defer r.pokeMu.Unlock()
	poked := r.poked
	r.poked = nil
	if poked == nil {
		poked = map[string]bool{}
	}
	return poked
}

// Pass looks at every business once: it reads its new events, then delivers what is due. It returns
// how many notices it made and how many phone notes a device took.
func (r *Reader) Pass(ctx context.Context) (made, sent int) {
	return r.pass(ctx, nil)
}

// pass is Pass for the businesses in only (by id), or for all of them when only is nil.
func (r *Reader) pass(ctx context.Context, only map[string]bool) (made, sent int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.businesses(ctx)
	if err != nil {
		r.Log.Printf("%s: can't list the businesses: %v", logPrefix, err)
		return 0, 0
	}
	for _, t := range list {
		if ctx.Err() != nil {
			break // stopping: start nothing new
		}
		if only != nil && !only[t.ID] {
			continue
		}
		m, s, err := r.business(ctx, t)
		if err != nil && ctx.Err() == nil {
			r.Log.Printf("%s %s: %v", logPrefix, t.Slug, err)
		}
		made, sent = made+m, sent+s
	}
	return made, sent
}

// business reads and delivers for one business, within its time. A panic (a bug while reading one
// event) becomes this business's error, so the others still get their phone notes; Postgres undoes
// the unfinished transaction.
func (r *Reader) business(ctx context.Context, t db.Tenant) (made, sent int, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, businessIn)
	defer cancel()
	if made, err = r.Read(readCtx, t); err != nil || r.Out == nil || !r.Push {
		return made, 0, err
	}
	sent, err = r.Deliver(ctx, t) // with its own time: what Read used is not taken from it
	return made, sent, err
}

// Settings are a business's phone-note settings.
type Settings struct {
	Name     string         // the business's name: in front of the line for a person in two businesses
	On       bool           // its set-up file says notifications: on
	Hours    hours.Hours    // its working hours for phone notes
	Calendar chase.Calendar // its time zone, days off and holidays
	PausedAt time.Time      // when an admin pressed Pause; zero when not paused
	PausedBy string         // that admin's person id
}

// Paused reports whether an admin paused phone notes for the business.
func (s Settings) Paused() bool { return !s.PausedAt.IsZero() }

// LoadSettings reads the current business's phone-note settings. tx must come from db.WithTenant.
func LoadSettings(ctx context.Context, tx pgx.Tx) (Settings, error) {
	var s Settings
	var zone, from, until string
	var daysOff []int16
	var pausedAt *time.Time
	var pausedBy *string
	if err := tx.QueryRow(ctx, `
		SELECT t.name, t.notifications, t.notify_from, t.notify_until, t.timezone, t.days_off,
			ARRAY(SELECT to_char(d, 'YYYY-MM-DD') FROM unnest(t.holidays) d ORDER BY d),
			b.paused_at, b.paused_by::text
		FROM tenants t LEFT JOIN notice_bookmark b ON b.tenant_id = t.id
		WHERE t.id = current_tenant()`).
		Scan(&s.Name, &s.On, &from, &until, &zone, &daysOff, &s.Calendar.Holidays, &pausedAt, &pausedBy); err != nil {
		return s, fmt.Errorf("notify: business settings: %w", err)
	}
	var err error
	if s.Hours, err = hours.Parse(from + "-" + until); err != nil {
		return s, err
	}
	if s.Calendar.Zone, err = time.LoadLocation(zone); err != nil {
		return s, fmt.Errorf("notify: time zone %q: %w", zone, err)
	}
	for _, d := range daysOff {
		s.Calendar.DaysOff = append(s.Calendar.DaysOff, time.Weekday(d))
	}
	if pausedAt != nil && pausedBy != nil {
		s.PausedAt, s.PausedBy = *pausedAt, *pausedBy
	}
	return s, nil
}

// bookmark returns where the reader got to in the current business's ledger (locked until tx ends,
// so two copies of the app never read the same events at once) and the ledger's end. A business seen
// for the first time starts at the end: turning phone notes on never tells anyone about old work.
func bookmark(ctx context.Context, tx pgx.Tx) (last, head int64, err error) {
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM ledger WHERE tenant_id = current_tenant()`).
		Scan(&head); err != nil {
		return 0, 0, fmt.Errorf("notify: ledger end: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO notice_bookmark (tenant_id, last_seq) VALUES (current_tenant(), $1)
		ON CONFLICT (tenant_id) DO NOTHING`, head); err != nil {
		return 0, 0, fmt.Errorf("notify: bookmark: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT last_seq FROM notice_bookmark WHERE tenant_id = current_tenant() FOR UPDATE`).
		Scan(&last); err != nil {
		return 0, 0, fmt.Errorf("notify: bookmark: %w", err)
	}
	return last, head, nil
}

func moveBookmark(ctx context.Context, tx pgx.Tx, to int64) error {
	_, err := tx.Exec(ctx, `UPDATE notice_bookmark SET last_seq = $1 WHERE tenant_id = current_tenant() AND last_seq < $1`, to)
	if err != nil {
		return fmt.Errorf("notify: move bookmark: %w", err)
	}
	return nil
}

// row is one ledger event as the reader reads it. xact is the database transaction that wrote it
// (Postgres's xmin): every event of one change has the same one.
type row struct {
	seq               int64
	at                time.Time
	kind, subject, by string
	body, xact        string
}

// interesting are the event kinds that can tell anyone (see Who).
var interesting = []string{"item.started", "item.assigned", "item.state_changed", "item.fields_set", "item.posted"}

// Read makes the business's notices from its new ledger events and moves its bookmark on, and
// returns how many it made. With phone notes off (the server's switch, the set-up file, an admin's
// pause) it only moves the bookmark to the ledger's end, so switching back on brings no flood. Pass
// runs it for every business, a poke for the poked ones.
func (r *Reader) Read(ctx context.Context, t db.Tenant) (int, error) {
	now := r.now()
	made := 0
	err := db.WithTenant(ctx, r.DB, t.ID, func(tx pgx.Tx) error {
		s, err := LoadSettings(ctx, tx)
		if err != nil {
			return err
		}
		last, head, err := bookmark(ctx, tx)
		if err != nil {
			return err
		}
		if !r.Push || !s.On || s.Paused() {
			return moveBookmark(ctx, tx, head)
		}
		members, err := workitem.Members(ctx, tx)
		if err != nil {
			return err
		}
		rd := reading{ctx: ctx, tx: tx, now: now, members: members, processes: map[string]*playbook.Playbook{},
			cause: map[string]string{}}
		if h := Hold(now, s.Hours, s.Calendar); !h.IsZero() {
			rd.hold = &h
		}
		for page := range maxPages {
			rows, err := events(ctx, tx, last)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break // the last page was exactly full: the bookmark still moves
			}
			full := len(rows) == pageSize
			if full && page == maxPages-1 {
				rows = wholeChanges(rows) // the next pass starts at the start of a change
			}
			n, err := rd.page(rows)
			if err != nil {
				return err
			}
			made += n
			last = rows[len(rows)-1].seq
			if !full {
				break
			}
		}
		return moveBookmark(ctx, tx, last)
	})
	if err != nil {
		made = 0
	}
	return made, err
}

// events reads a page of the current business's ledger events after seq.
func events(ctx context.Context, tx pgx.Tx, after int64) ([]row, error) {
	rs, err := tx.Query(ctx, `
		SELECT seq, occurred_at, kind, subject_type, coalesce(actor_person_id::text, on_behalf_of::text, ''), body, xmin::text
		FROM ledger WHERE tenant_id = current_tenant() AND seq > $1 ORDER BY seq LIMIT $2`, after, pageSize)
	if err != nil {
		return nil, fmt.Errorf("notify: read the ledger: %w", err)
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var e row
		if err := rs.Scan(&e.seq, &e.at, &e.kind, &e.subject, &e.by, &e.body, &e.xact); err != nil {
			return nil, fmt.Errorf("notify: read the ledger: %w", err)
		}
		out = append(out, e)
	}
	return out, rs.Err()
}

// wholeChanges leaves out the events at the end of a full page that belong to the same change as the
// last one, so a change is never read in two passes (its person would be missed). A page that is all
// one change is kept whole.
func wholeChanges(rows []row) []row {
	last := rows[len(rows)-1].xact
	i := len(rows)
	for i > 0 && rows[i-1].xact == last {
		i--
	}
	if i == 0 {
		return rows
	}
	return rows[:i]
}

// reading is one business's read, within its transaction.
type reading struct {
	ctx       context.Context
	tx        pgx.Tx
	now       time.Time
	hold      *time.Time // made in quiet hours: when the notices may go
	members   []workitem.Member
	processes map[string]*playbook.Playbook // by version id
	// cause is, for each database transaction (xmin) read so far in this pass, the first person who
	// acted in it: the person whose change it is. A change may go on over two pages.
	cause map[string]string
}

// page makes the notices for a page of events. It returns how many it added.
func (rd reading) page(rows []row) (int, error) {
	// The person whose change each event is part of: Seyal writes many events on its own inside a
	// person's change (the first holder of an item Ben starts, the step's owner after Dev's move), and
	// that person must not be told about them either (D88, item 2).
	for _, e := range rows {
		if e.by != "" && rd.cause[e.xact] == "" {
			rd.cause[e.xact] = e.by
		}
	}
	made := 0
	for _, e := range rows {
		if e.subject != "work_item" || !slices.Contains(interesting, e.kind) || rd.now.Sub(e.at) > MaxAge {
			continue
		}
		var b struct {
			Payload struct {
				Change map[string]any `json:"change"`
				Item   workitem.Item  `json:"item"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(e.body), &b); err != nil {
			return made, fmt.Errorf("notify: event %d: %w", e.seq, err)
		}
		ev := Event{Seq: e.seq, Kind: e.kind, By: e.by, Cause: rd.cause[e.xact], Change: b.Payload.Change, Item: b.Payload.Item}
		if ev.Kind == "item.posted" && ev.Change["chase"] == nil {
			continue // Seyal's own thread lines tell nobody; only the chaser's do
		}
		p, err := rd.process(ev.Item.PlaybookVersionID)
		if err != nil {
			return made, err
		}
		var st engine.ApprovalStatus
		if p.Approvals != nil && ev.Kind != "item.assigned" {
			if st, err = workitem.StatusOf(rd.ctx, rd.tx, p, ev.Item); err != nil {
				return made, err
			}
		}
		for _, n := range Who(ev, p, rd.members, st) {
			added, err := rd.add(n, e.at)
			if err != nil {
				return made, err
			}
			made += added
		}
	}
	return made, nil
}

func (rd reading) process(versionID string) (*playbook.Playbook, error) {
	if p := rd.processes[versionID]; p != nil {
		return p, nil
	}
	p, err := workitem.PlaybookFor(rd.ctx, rd.tx, workitem.Item{PlaybookVersionID: versionID})
	if err != nil {
		return nil, err
	}
	rd.processes[versionID] = p
	return p, nil
}

// add keeps one notice. The same person, event and kind is kept once.
func (rd reading) add(n Notice, at time.Time) (int, error) {
	tag, err := rd.tx.Exec(rd.ctx, `
		INSERT INTO notices (tenant_id, person_id, kind, item_id, number, event_seq, event_at, from_person,
			team, slots, batch, made_at, hold_until)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, nullif($7::text, '')::uuid, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, person_id, event_seq, kind) DO NOTHING`,
		n.Person, n.Kind, n.Item, n.Number, n.Seq, at, n.From, n.Team, nonNil(n.Slots), n.Batch, rd.now, rd.hold)
	if err != nil {
		return 0, fmt.Errorf("notify: keep a notice: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// Pause pauses phone notes for the current business (paused true), or resumes them, as the admin by:
// Settings → Processes → Leaves Seyal (design 2.4). It is a business.updated ledger event by the
// admin, written in the same transaction. It reports whether anything changed. The file's
// notifications: off is separate: both must allow phone notes for any to go. tx must come from
// db.WithTenant.
func Pause(ctx context.Context, tx pgx.Tx, paused bool, by string) (bool, error) {
	if by == "" {
		return false, errors.New("notify: pause: nobody pressed it")
	}
	// The bookmark first, then the ledger head (ledger.Lock). A reader's Read holds the bookmark until
	// it ends, and each phone note's hand-over holds it shared until its mark is saved, so Pause may
	// wait for them; then no later phone note goes, as the page says. It waits before it holds the
	// ledger head, so nobody else's change waits too. Writers never take the bookmark, and the reader
	// never takes the ledger head, so the two locks can't wait for each other.
	if _, _, err := bookmark(ctx, tx); err != nil {
		return false, err
	}
	if err := ledger.Lock(ctx, tx); err != nil {
		return false, err
	}
	var was bool
	var tenantID string
	if err := tx.QueryRow(ctx, `SELECT paused_at IS NOT NULL, tenant_id::text FROM notice_bookmark WHERE tenant_id = current_tenant()`).
		Scan(&was, &tenantID); err != nil {
		return false, fmt.Errorf("notify: pause: %w", err)
	}
	if was == paused {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE notice_bookmark SET paused_at = CASE WHEN $1 THEN now() END, paused_by = CASE WHEN $1 THEN $2::uuid END
		WHERE tenant_id = current_tenant()`, paused, by); err != nil {
		return false, fmt.Errorf("notify: pause: %w", err)
	}
	_, err := ledger.Append(ctx, tx, ledger.Event{Kind: "business.updated", SubjectType: "business", SubjectID: tenantID,
		Payload: map[string]any{"changes": map[string]any{"phone_notes_paused": map[string]any{"old": was, "new": paused}}}}.
		By(ledger.PersonActor(by)))
	return err == nil, err
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reader) businesses(ctx context.Context) ([]db.Tenant, error) {
	if r.List != nil {
		return r.List(ctx)
	}
	return db.Tenants(ctx, r.DB)
}

// itemPath and myWork are where a tap goes.
func itemPath(slug, id string) string { return "/t/" + slug + "/items/" + id }
func myWork(slug string) string       { return "/t/" + slug + "/" }
