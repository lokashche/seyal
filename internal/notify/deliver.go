// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Why a waiting notice is dropped instead of sent (the notices.result column).
const (
	Sent     = "sent"
	Seen     = "seen"     // the person opened the item after the event
	Inactive = "inactive" // the person is no longer active here, or has no login
	Closed   = "closed"   // nothing more can happen to the item (for example it was cancelled)
	Decided  = "decided"  // the approval slots it was about are no longer waiting
	Stale    = "stale"    // it waited more than MaxAge (the server was down, or phone notes were off)
)

// waiting is a notices row that waits to go.
type waiting struct {
	ID                 int64
	Person, Kind, Item string
	Number             string
	Seq                int64
	At                 time.Time // when the event happened
	From, Team         string
	Slots              []string
	Batch              string
	Made               time.Time
	Hold               *time.Time // made in quiet hours: part of the morning note
}

// Deliver turns the business's due notices into phone notes: it drops those that no longer matter,
// groups the rest per person, keeps to the hourly cap, and hands each phone note to r.Out, which must
// be set. Each person's phone note is its own transaction, committed before the next person's
// begins: it keeps that person's notices locked while the note is handed on, and marks them sent only
// once r.Out took it. So a later failure (the next person's note, a slow push service, a bug) never
// undoes the mark of a note already handed on, and a note r.Out didn't take waits for the next pass
// (see Sender). Each person's transaction also holds the bookmark row shared, so an admin's Pause
// waits for the note on its way and no later one goes. Outside working hours it hands on nothing, and
// what waits is held for the next working morning, so the hourly cap or a late restart never buzzes a
// phone at night. It returns how many it handed on.
func (r *Reader) Deliver(ctx context.Context, t db.Tenant) (int, error) {
	if r.Out == nil {
		return 0, errors.New("notify: deliver: no sender")
	}
	now := r.now()
	started := time.Now() // the time limits are real time, whatever the reader's clock says
	// Who has notices due, in a short transaction of its own. Each transaction has its own time, and a
	// stop (ctx ending) doesn't cut it short: what was handed on before the stop is still marked sent.
	// A stop only ends the hand-overs.
	var people []string
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), personIn)
	err := db.WithTenant(txCtx, r.DB, t.ID, func(tx pgx.Tx) error {
		s, err := LoadSettings(txCtx, tx)
		if err != nil || !s.On || s.Paused() {
			return err
		}
		if morning := Hold(now, s.Hours, s.Calendar); !morning.IsZero() {
			return holdForMorning(txCtx, tx, now, morning)
		}
		rows, _ := tx.Query(txCtx, `SELECT DISTINCT person_id::text FROM notices
			WHERE tenant_id = current_tenant() AND result IS NULL AND (hold_until IS NULL OR hold_until <= $1)
			ORDER BY 1`, now)
		if people, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("notify: who has notices due: %w", err)
		}
		return nil
	})
	cancel()
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, person := range people {
		if ctx.Err() != nil || time.Since(started) > businessIn-personIn {
			break // stopping, or out of time: the rest wait for the next pass
		}
		took, stop, err := r.deliverTo(ctx, t, person, now)
		if took {
			sent++
		}
		if err != nil {
			return sent, err // the notes already handed on stay marked sent; the rest wait
		}
		if stop {
			break
		}
	}
	return sent, nil
}

// deliverTo makes and hands on one person's phone note, in a transaction of its own. stop is true
// when no more phone notes may go in this pass: the business's notes were paused or switched off, its
// working hours ended, the server is stopping, or the time ran out.
func (r *Reader) deliverTo(ctx context.Context, t db.Tenant, person string, now time.Time) (took, stop bool, err error) {
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), personIn)
	defer cancel()
	err = db.WithTenant(txCtx, r.DB, t.ID, func(tx pgx.Tx) error {
		// The bookmark row, shared, before the settings are read: Pause changes that row, so a press
		// waits until this note is handed on and marked, and the next person's transaction sees it.
		// (No row: the reader never read this business, so it has no notices.)
		if _, err := tx.Exec(txCtx, `SELECT 1 FROM notice_bookmark WHERE tenant_id = current_tenant() FOR SHARE`); err != nil {
			return fmt.Errorf("notify: bookmark: %w", err)
		}
		s, err := LoadSettings(txCtx, tx)
		if err != nil {
			return err
		}
		if !s.On || s.Paused() || !Hold(now, s.Hours, s.Calendar).IsZero() {
			stop = true
			return nil
		}
		list, err := due(txCtx, tx, person, now)
		if err != nil || len(list) == 0 {
			return err
		}
		d := delivery{ctx: txCtx, tx: tx, now: now, items: map[string]*current{}}
		if err := d.load(); err != nil {
			return err
		}
		var keep []waiting
		dropped := map[string][]int64{} // by reason
		for _, w := range list {
			why, err := d.drop(w)
			if err != nil {
				return err
			}
			if why != "" {
				dropped[why] = append(dropped[why], w.ID)
				continue
			}
			keep = append(keep, w)
		}
		for why, ids := range dropped {
			if _, err := tx.Exec(txCtx, `UPDATE notices SET result = $2 WHERE tenant_id = current_tenant() AND id = ANY($1)`,
				ids, why); err != nil {
				return fmt.Errorf("notify: drop: %w", err)
			}
		}
		if len(keep) == 0 {
			return nil
		}
		var sentLastHour int
		if err := tx.QueryRow(txCtx, `SELECT count(DISTINCT note_id) FROM notices
			WHERE tenant_id = current_tenant() AND person_id::text = $1 AND result = 'sent' AND sent_at > $2`,
			person, now.Add(-time.Hour)).Scan(&sentLastHour); err != nil {
			return fmt.Errorf("notify: cap: %w", err)
		}
		if sentLastHour >= MaxPerHour {
			return nil // they wait, and go together once the hour has room again
		}
		business, err := d.businessName(person, s.Name)
		if err != nil {
			return err
		}
		n := compose(keep, d.names, d.teams, business)
		n.Tenant, n.Person = t.ID, person
		n.Path = myWork(t.Slug)
		if n.item != "" {
			n.Path = itemPath(t.Slug, n.item)
		}
		n.Tag = tag(t.ID, person, n.item)
		// The hand-over may take all of sendIn; markIn is kept free after it to save the mark.
		if ctx.Err() != nil || timeLeft(txCtx) < sendIn+markIn {
			stop = true
			return nil // stopping, or out of time: the note waits for the next pass
		}
		took, err = r.handOn(ctx, txCtx, tx, t, n.Note, now)
		return err
	})
	return took && err == nil, stop, err
}

// handOn hands one phone note to r.Out, with a time of its own, and marks its notices: sent when
// r.Out took it, otherwise one more try (they wait for the next pass).
func (r *Reader) handOn(ctx, txCtx context.Context, tx pgx.Tx, t db.Tenant, n Note, now time.Time) (bool, error) {
	sendCtx, cancel := context.WithTimeout(ctx, sendIn)
	err := r.Out.Send(sendCtx, n)
	cancel()
	if err != nil {
		r.Log.Printf("%s %s: a phone note wasn't handed on, and waits: %v", logPrefix, t.Slug, err)
		if _, err := tx.Exec(txCtx, `UPDATE notices SET tries = tries + 1 WHERE tenant_id = current_tenant() AND id = ANY($1)`,
			n.Notices); err != nil {
			return false, fmt.Errorf("notify: count a try: %w", err)
		}
		return false, nil
	}
	if _, err := tx.Exec(txCtx, `UPDATE notices SET result = 'sent', sent_at = $2, note_id = $3
		WHERE tenant_id = current_tenant() AND id = ANY($1)`, n.Notices, now, n.Notices[0]); err != nil {
		return false, fmt.Errorf("notify: mark sent: %w", err)
	}
	return true, nil
}

// timeLeft is how long ctx has before it ends.
func timeLeft(ctx context.Context) time.Duration {
	if end, ok := ctx.Deadline(); ok {
		return time.Until(end)
	}
	return businessIn
}

// holdForMorning is Deliver outside working hours: the waiting notices that have no hold yet (the
// hourly cap kept them, or the server stopped before they went) wait for the next working morning
// and go in the morning note. Those already too old are left, to be dropped as stale; so are those
// whose morning has passed (the server was down then): they never go at night.
func holdForMorning(ctx context.Context, tx pgx.Tx, now, morning time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE notices SET hold_until = $1
		WHERE tenant_id = current_tenant() AND result IS NULL AND hold_until IS NULL AND made_at > $2`,
		morning, now.Add(-MaxAge)); err != nil {
		return fmt.Errorf("notify: hold for the morning: %w", err)
	}
	return nil
}

// due returns the person's notices that may go now, oldest first, and locks them (rows another copy of
// the app is working on are skipped).
func due(ctx context.Context, tx pgx.Tx, person string, now time.Time) ([]waiting, error) {
	rows, _ := tx.Query(ctx, `
		SELECT id, person_id::text, kind, item_id::text, number, event_seq, event_at, coalesce(from_person::text, ''),
			team, slots, batch, made_at, hold_until
		FROM notices
		WHERE tenant_id = current_tenant() AND person_id::text = $2 AND result IS NULL
			AND (hold_until IS NULL OR hold_until <= $1)
		ORDER BY id
		FOR UPDATE SKIP LOCKED`, now, person)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[waiting])
	if err != nil {
		return nil, fmt.Errorf("notify: due notices: %w", err)
	}
	return list, nil
}

// delivery is one business's deliver, within its transaction.
type delivery struct {
	ctx     context.Context
	tx      pgx.Tx
	now     time.Time
	members map[string]bool   // the active people with a login, by id
	names   map[string]string // every person's name, by id
	teams   map[string]string // role names, by key
	items   map[string]*current
}

// current is an item as it is now, with its process and approval status.
type current struct {
	it workitem.Item
	p  *playbook.Playbook
	st *engine.ApprovalStatus // worked out when first needed
}

func (d *delivery) load() error {
	members, err := workitem.Members(d.ctx, d.tx)
	if err != nil {
		return err
	}
	d.members = map[string]bool{}
	for _, m := range members {
		d.members[m.ID] = true
	}
	if d.names, err = pairs(d.ctx, d.tx, `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant()`); err != nil {
		return err
	}
	d.teams, err = pairs(d.ctx, d.tx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	return err
}

func pairs(ctx context.Context, tx pgx.Tx, query string) (map[string]string, error) {
	rows, _ := tx.Query(ctx, query)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ K, V string }])
	if err != nil {
		return nil, fmt.Errorf("notify: names: %w", err)
	}
	out := make(map[string]string, len(list))
	for _, p := range list {
		out[p.K] = p.V
	}
	return out, nil
}

// drop says why the notice no longer matters, or "" when it should go.
func (d *delivery) drop(w waiting) (string, error) {
	start := w.Made
	if w.Hold != nil {
		start = *w.Hold
	}
	if d.now.Sub(start) > MaxAge {
		return Stale, nil
	}
	if !d.members[w.Person] {
		return Inactive, nil
	}
	c, err := d.item(w.Item)
	if err != nil {
		return "", err
	}
	if engine.IsClosed(c.p, c.it.State) {
		return Closed, nil
	}
	var seen *time.Time
	err = d.tx.QueryRow(d.ctx, `SELECT seen_at FROM item_seen WHERE tenant_id = current_tenant() AND person_id::text = $1 AND item_id::text = $2`,
		w.Person, w.Item).Scan(&seen)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("notify: seen: %w", err)
	}
	if seen != nil && !seen.Before(w.At) {
		return Seen, nil
	}
	if len(w.Slots) > 0 {
		a := c.p.Approvals
		if a == nil || c.it.State != a.In {
			return Decided, nil
		}
		if c.st == nil {
			st, err := workitem.StatusOf(d.ctx, d.tx, c.p, c.it)
			if err != nil {
				return "", err
			}
			c.st = &st
		}
		if !slices.ContainsFunc(w.Slots, func(s string) bool { return slices.Contains(c.st.Waiting, s) }) {
			return Decided, nil
		}
	}
	return "", nil
}

func (d *delivery) item(id string) (*current, error) {
	if c := d.items[id]; c != nil {
		return c, nil
	}
	it, err := workitem.Get(d.ctx, d.tx, id)
	if err != nil {
		return nil, err
	}
	p, err := workitem.PlaybookFor(d.ctx, d.tx, it)
	if err != nil {
		return nil, err
	}
	c := &current{it: it, p: p}
	d.items[id] = c
	return c, nil
}

// businessName is the business's name when the person's login belongs to two or more active
// businesses (decision 2), and "" otherwise. auth.Businesses looks at the login's own people only.
func (d *delivery) businessName(person, name string) (string, error) {
	var account *string
	if err := d.tx.QueryRow(d.ctx, `SELECT account_id::text FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
		person).Scan(&account); err != nil {
		return "", fmt.Errorf("notify: login: %w", err)
	}
	if account == nil {
		return "", nil
	}
	bs, err := auth.Businesses(d.ctx, d.tx, *account)
	if err != nil {
		return "", err
	}
	active := 0
	for _, b := range bs {
		if b.Active {
			active++
		}
	}
	if active < 2 {
		return "", nil
	}
	return name, nil
}

// composed is a Note before the business's ids are filled in, with the one item it is about ("" when
// it is about several: then a tap opens My Work).
type composed struct {
	Note
	item string
}

// compose makes one person's phone note from their notices that go now (at least one, oldest first):
//   - one notice: its own line ("New work from Ben: J-2026-0007"), opening the item;
//   - all from one admin's bulk move: "30 items moved to you by Chitra";
//   - any made in quiet hours: the morning note, "While it was quiet: 2 reminders, 1 new work";
//   - otherwise: "3 new things for you".
//
// Several notices open the item only when they are all about the same one; otherwise My Work.
func compose(list []waiting, names, teams map[string]string, business string) composed {
	n := composed{Note: Note{TTL: TTL, Urgency: "normal"}}
	for _, w := range list {
		n.Notices = append(n.Notices, w.ID)
	}
	first := list[0]
	if !slices.ContainsFunc(list, func(w waiting) bool { return w.Item != first.Item }) {
		n.item = first.Item
	}
	held := slices.ContainsFunc(list, func(w waiting) bool { return w.Hold != nil })
	if held {
		n.TTL = MorningTTL
	}
	oneBatch := first.Batch != "" && !slices.ContainsFunc(list, func(w waiting) bool { return w.Batch != first.Batch || w.Kind != NewWork })
	switch {
	case len(list) == 1:
		n.Body = Text(first.Kind, Words{Business: business, Number: first.Number, From: names[first.From],
			Team: cmp.Or(teams[first.Team], first.Team)})
	case oneBatch:
		n.Body = Moved(len(list), names[first.From], business)
	case held:
		counts := map[string]int{}
		for _, w := range list {
			counts[w.Kind]++
		}
		n.Body = Quiet(counts, business)
	default:
		n.Body = Several(len(list), business)
	}
	return n
}

// tag is a phone note's Topic and the browser's tag: 32 letters, digits, "-" and "_" (what RFC 8030
// allows a Topic), made from the business, the person and the item (or My Work). The push service
// sees it, so it is never the item's readable number; the same item gives the same tag, so a newer
// phone note about it replaces an older one the phone hasn't fetched yet.
func tag(tenant, person, item string) string {
	sum := sha256.Sum256([]byte("seyal phone note\x00" + tenant + "\x00" + person + "\x00" + item))
	return base64.RawURLEncoding.EncodeToString(sum[:24])
}
