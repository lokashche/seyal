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
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/auth"
	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/playbook"
	"seyal/internal/push"
	"seyal/internal/workitem"
)

// Why a waiting notice is dropped instead of sent (the notices.result column).
const (
	Sent     = "sent"
	Seen     = "seen"      // the person opened the item after the event
	Inactive = "inactive"  // the person is no longer active here, or has no login
	Closed   = "closed"    // nothing more can happen to the item (for example it was cancelled)
	Decided  = "decided"   // the approval slots it was about are no longer waiting
	Stale    = "stale"     // it waited more than MaxAge (the server was down, or phone notes were off)
	NoDevice = "no_device" // the person has no device with phone notes on: nothing was sent
	Failed   = "failed"    // no device took it, and it won't be tried again
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
	Tries              int        // posts that no device took
}

// Deliver turns the business's due notices into phone notes: it drops those that no longer matter,
// groups the rest per person, keeps to the hourly cap, and posts each phone note to the person's
// devices through r.Out, which must be set (sender.go). Each person's phone note is its own
// transaction, committed before the next person's begins: it keeps that person's notices locked
// (FOR UPDATE SKIP LOCKED, so another copy of the app never takes them too) while the note is posted,
// and marks them sent only once a device's push service took it. So a later failure (the next person's
// note, a slow push service, a bug) never undoes the mark of a note already posted, and a note no
// device took waits for its next try. Each person's transaction also holds the bookmark row shared,
// so an admin's Pause waits for the note on its way and no later one goes. Outside working hours it
// posts nothing, and what waits is held for the next working morning, so the hourly cap or a late
// restart never buzzes a phone at night. It returns how many phone notes a device took.
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
				AND (next_try_at IS NULL OR next_try_at <= $1)
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
	rd := &round{}
	defer r.endRefused(ctx, t, rd) // also after a failure: the answers already came
	for _, person := range people {
		if ctx.Err() != nil || time.Since(started) > businessIn-personIn {
			break // stopping, or out of time: the rest wait for the next pass
		}
		took, stop, err := r.deliverTo(ctx, t, person, now, rd)
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

// endRefused ends the devices whose push service said 401 or 403 in this pass, unless the answers look
// like Seyal's own key is wrong: two or more people had a device refused, and they are more than half
// of the people posted to (then none ends), or a device's own push service (pushFamily) took no note
// since the server started (then that device doesn't end). For those it logs "check the SEYAL_PUSH
// keys", so a mistake in .env doesn't end everyone's phone notes, not even one push service's devices.
// The first part counts people, not devices: Seyal can't check which key a browser was asked to use, so
// one login could turn on five devices with a key of its own, all refused. Counted as devices, those
// would hold the guard and raise the alarm for the whole server; counted as one person, they can't, and
// they end like any other refused device.
func (r *Reader) endRefused(ctx context.Context, t db.Tenant, rd *round) {
	n := len(rd.refused)
	if n == 0 {
		return
	}
	people := map[string]bool{}
	for _, d := range rd.refused {
		people[d.PersonID] = true
	}
	if len(people) >= 2 && 2*len(people) > rd.people {
		r.Log.Printf("%s %s: push services refused Seyal's key for %d of %d devices (%d of %d people): check the SEYAL_PUSH keys (no device was ended)",
			logPrefix, t.Slug, n, rd.tried, len(people), rd.people)
		return
	}
	var end []deviceAnswer
	for _, d := range rd.refused {
		if _, ok := r.keyWorks.Load(pushFamily(d.Device.Endpoint)); ok {
			end = append(end, deviceAnswer{d: d, why: "no longer accepts Seyal's key for it (401 or 403)"})
		}
	}
	if held := n - len(end); held > 0 {
		r.Log.Printf("%s %s: push services refused Seyal's key for %d of %d devices (%d of %d people): check the SEYAL_PUSH keys "+
			"(%d device(s) not ended: their push service has taken no phone note since the server started)",
			logPrefix, t.Slug, n, rd.tried, len(people), rd.people, held)
	}
	r.devicesAnswered(ctx, t, end)
}

// deviceAnswer is what a push service's answer means for one device row: ok, it took a note
// (last_ok_at); otherwise the device ends ("gone"), for why (for the log).
type deviceAnswer struct {
	d   auth.BusinessDevice
	ok  bool
	why string
}

// devicesAnswered saves what the push services said about devices, in a short transaction of its own.
// It runs after the person's notices are marked and committed, so a failure here (a database error, a
// device row locked by its login turning phone notes on just then) is only logged: it never undoes a
// mark, so a note a device took is never posted again because of it.
func (r *Reader) devicesAnswered(ctx context.Context, t db.Tenant, list []deviceAnswer) {
	if len(list) == 0 {
		return
	}
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), personIn)
	defer cancel()
	err := db.WithTenant(txCtx, r.DB, t.ID, func(tx pgx.Tx) error {
		for _, a := range list {
			if err := auth.DeviceAnswered(txCtx, tx, a.d.PersonID, a.d.DeviceID, a.ok); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.Log.Printf("%s %s: can't save what the push services said about %d device(s): %v", logPrefix, t.Slug, len(list), err)
		return
	}
	for _, a := range list {
		if !a.ok {
			r.Log.Printf("%s %s: a device ended: its push service %s", logPrefix, t.Slug, a.why)
		}
	}
}

// deliverTo makes and hands on one person's phone note, in a transaction of its own. stop is true
// when no more phone notes may go in this pass: the business's notes were paused or switched off, its
// working hours ended, the server is stopping, or the time ran out.
func (r *Reader) deliverTo(ctx context.Context, t db.Tenant, person string, now time.Time, rd *round) (took, stop bool, err error) {
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), personIn)
	defer cancel()
	var answered []deviceAnswer
	// The device rows change after the person's transaction ends (also when it failed: the push
	// services did answer), never inside it, so they can't undo the notices' mark.
	defer func() { r.devicesAnswered(ctx, t, answered) }()
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
		devices, err := devicesOf(txCtx, tx, person)
		if err != nil {
			return err
		}
		if len(devices) == 0 {
			// Nobody to post to: the person never turned phone notes on, or every device ended.
			ids := make([]int64, len(keep))
			for i, w := range keep {
				ids[i] = w.ID
			}
			if _, err := tx.Exec(txCtx, `UPDATE notices SET result = $2 WHERE tenant_id = current_tenant() AND id = ANY($1)`,
				ids, NoDevice); err != nil {
				return fmt.Errorf("notify: no device: %w", err)
			}
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
		tries := 0
		for _, w := range keep {
			tries = max(tries, w.Tries)
		}
		took, answered, err = r.handOn(ctx, txCtx, tx, t, n.Note, devices, tries, now, rd)
		return err
	})
	return took && err == nil, stop, err
}

// devicesOf returns the person's devices that get phone notes, through the one narrow look-up
// (auth.BusinessDevices, push_devices_here() in migration 00024).
func devicesOf(ctx context.Context, tx pgx.Tx, person string) ([]auth.BusinessDevice, error) {
	all, err := auth.BusinessDevices(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("notify: devices: %w", err)
	}
	return slices.DeleteFunc(all, func(d auth.BusinessDevice) bool { return d.PersonID != person }), nil
}

// handOn posts one phone note to each of the person's devices at once, all within sendIn, reads what
// each push service said (sender.go), and marks the notices: sent when a device took it; otherwise,
// if a push service had a problem, one more try and the time of the next; otherwise (or after
// MaxTries) failed. tries is the most tries any of the notices had so far. It returns what the
// answers mean for the device rows, for devicesAnswered to save once this transaction has ended: here
// they would come between the post and the mark, and a failure there would undo the mark.
func (r *Reader) handOn(ctx, txCtx context.Context, tx pgx.Tx, t db.Tenant, n Note, devices []auth.BusinessDevice,
	tries int, now time.Time, rd *round) (bool, []deviceAnswer, error) {
	answers := r.post(ctx, n, devices)
	rd.tried += len(devices)
	rd.people++ // one phone note per person per pass
	tookIt, again := false, false
	var asked time.Duration
	var whys []string
	var rows []deviceAnswer
	for i, a := range answers {
		d := devices[i]
		switch a.outcome {
		case took:
			tookIt = true
			r.keyWorks.Store(pushFamily(d.Device.Endpoint), true)
			rows = append(rows, deviceAnswer{d: d, ok: true})
			continue
		case badKey:
			rd.refused = append(rd.refused, d)
			again = true
		case gone:
			rows = append(rows, deviceAnswer{d: d, why: "says the address is gone (" + a.why + ")"})
		case tryAgain:
			again = true
			asked = max(asked, a.wait)
		case noRetry:
			r.Log.Printf("%s %s: a push service refused a phone note (%s); that answer alone brings no retry", logPrefix, t.Slug, a.why)
		}
		whys = append(whys, a.why)
	}
	if tookIt {
		if _, err := tx.Exec(txCtx, `UPDATE notices SET result = 'sent', sent_at = $2, note_id = $3, next_try_at = NULL
			WHERE tenant_id = current_tenant() AND id = ANY($1)`, n.Notices, now, n.Notices[0]); err != nil {
			return false, rows, fmt.Errorf("notify: mark sent: %w", err)
		}
		return true, rows, nil
	}
	why := strings.Join(slices.Compact(slices.Sorted(slices.Values(whys))), ", ")
	if next, ok := nextTry(tries+1, asked, now); again && ok {
		r.Log.Printf("%s %s: a phone note wasn't handed on (%s), and waits until %s", logPrefix, t.Slug, why, next.UTC().Format(time.TimeOnly))
		if _, err := tx.Exec(txCtx, `UPDATE notices SET tries = tries + 1, next_try_at = $2 WHERE tenant_id = current_tenant() AND id = ANY($1)`,
			n.Notices, next); err != nil {
			return false, rows, fmt.Errorf("notify: count a try: %w", err)
		}
		return false, rows, nil
	}
	r.Log.Printf("%s %s: a phone note wasn't handed on (%s), and is given up", logPrefix, t.Slug, why)
	if _, err := tx.Exec(txCtx, `UPDATE notices SET tries = tries + 1, next_try_at = NULL, result = $2
		WHERE tenant_id = current_tenant() AND id = ANY($1)`, n.Notices, Failed); err != nil {
		return false, rows, fmt.Errorf("notify: give up: %w", err)
	}
	return false, rows, nil
}

// post posts n to every device at once, each through r.Out, all within sendIn, and returns what each
// push service said, in the order of devices. A panic in one post (a bug) comes back here, so the
// person's transaction is undone and the business's pass reports it.
func (r *Reader) post(ctx context.Context, n Note, devices []auth.BusinessDevice) []answer {
	sendCtx, cancel := context.WithTimeout(ctx, sendIn)
	defer cancel()
	answers := make([]answer, len(devices))
	panics := make([]any, len(devices))
	var wg sync.WaitGroup
	for i, d := range devices {
		wg.Go(func() {
			defer func() { panics[i] = recover() }()
			payload, err := n.payload(d.Origin)
			if err != nil {
				answers[i] = answer{outcome: noRetry, why: "could not write it"}
				return
			}
			a, err := r.Out.Send(sendCtx, d.Device, push.Message{Payload: payload, TTL: n.TTL, Urgency: n.Urgency, Topic: n.Tag})
			answers[i] = classify(a, err, time.Now())
		})
	}
	wg.Wait()
	for _, p := range panics {
		if p != nil {
			panic(p)
		}
	}
	return answers
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
			team, slots, batch, made_at, hold_until, tries
		FROM notices
		WHERE tenant_id = current_tenant() AND person_id::text = $2 AND result IS NULL
			AND (hold_until IS NULL OR hold_until <= $1) AND (next_try_at IS NULL OR next_try_at <= $1)
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
