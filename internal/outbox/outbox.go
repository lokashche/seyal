// SPDX-License-Identifier: AGPL-3.0-or-later

// Package outbox posts the sends a process queued (`do: send`, Phase 4 part 4c-3, D96) to the other
// apps they are for. It is its own loop next to the web server, not a chaser task: it runs every minute
// and soon after a poke, also when the chaser is off or a business's chasing is paused.
//
// Each pass, for each business:
//
//  1. In a short transaction it looks at the sends that are due, oldest first (FOR UPDATE SKIP LOCKED).
//     A send not gone within 24 hours of being queued expires. One whose other app the set-up file no
//     longer lists, or may no longer receive its fields, is given up. One that can't go now (sending is
//     off on this server, the host isn't allowed, the app is paused) waits, with the reason kept, and is
//     looked at again a minute later (so a pile of waiting sends never blocks the others). The
//     rest are claimed: one more try is counted and the send is held for a few minutes, so nothing posts
//     it twice. More than 30 sends to one app in an hour pauses that app instead.
//  2. With no transaction open, it posts each claimed send, signed, through the safe outgoing caller.
//     Just before each post it reads the other app again, in a short transaction: a send whose app was
//     paused (by an admin, or by Seyal after an earlier send of this pass), taken out of the set-up
//     file, narrowed or blocked since the claim doesn't go. It is let go (released) for the next pass,
//     which ends it or keeps it waiting as in step 1.
//  3. In a short transaction per send, it records the answer: done (item.send_done); try again after
//     5 minutes, 30 minutes, 2 hours and 6 hours (longer if the app asks), then give up; 410 gives up
//     and pauses the app; anything an admin must fix gives up at once (item.send_failed).
//
// Only the sends row keeps each try. The ledger gets the queueing, and how it ended. No address, secret
// or Go error text is ever logged or stored: only the safe caller's fixed short reasons.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/send"
	"seyal/internal/workitem"
)

// The sender's numbers (design 2.3, "How a send works").
const (
	Every       = time.Minute      // how often a full pass runs
	PokeWait    = 2 * time.Second  // how long after a poke the poked businesses' pass runs
	MaxTries    = 5                // the first try and four more
	ExpireAfter = 24 * time.Hour   // a send not gone within this long of being queued expires
	MaxAsked    = 6 * time.Hour    // the longest wait an app's Retry-After gets
	Hold        = 10 * time.Minute // how long a claimed send is held: longer than a batch of calls takes
	Batch       = 10               // the most sends claimed for one business in one pass
)

// RetryWaits are the waits after the first, second, third and fourth try that didn't get through.
var RetryWaits = []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// Sender posts queued sends. Make one with New.
type Sender struct {
	DB    db.Beginner // the app pool: the app role, row-level security on
	Log   *log.Logger
	On    bool                                           // the server's switch (SEYAL_SEND=on); off, sends wait and then expire
	Hosts send.Hosts                                     // the hosts the server allows (SEYAL_SEND_HOSTS)
	Out   send.Poster                                    // the safe outgoing caller, or a fake in tests
	List  func(ctx context.Context) ([]db.Tenant, error) // the businesses; nil: all of them
	Now   func() time.Time                               // the clock; nil: time.Now (tests use another)

	mu     sync.Mutex // one pass at a time
	pokeMu sync.Mutex
	poked  map[string]bool
	pokes  chan struct{}
}

// New returns a sender for the app pool. on is SEYAL_SEND; out posts (nil: the safe caller for hosts).
func New(pool db.Beginner, l *log.Logger, on bool, hosts send.Hosts, out send.Poster) *Sender {
	if out == nil {
		out = send.NewAppCaller(hosts)
	}
	return &Sender{DB: pool, Log: l, On: on, Hosts: hosts, Out: out}
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Poke asks for a pass for one business soon (a web request calls it after a change, so an approval's
// send goes within seconds). It never blocks, and does nothing while sending is off on the server.
func (s *Sender) Poke(tenantID string) {
	if s == nil || !s.On {
		return
	}
	s.pokeMu.Lock()
	if s.poked == nil {
		s.poked = map[string]bool{}
	}
	s.poked[tenantID] = true
	pokes := s.pokeChan()
	s.pokeMu.Unlock()
	select {
	case pokes <- struct{}{}:
	default: // a poke already waits: it covers this one too
	}
}

// pokeChan returns the poke channel, made on first use. s.pokeMu must be held.
func (s *Sender) pokeChan() chan struct{} {
	if s.pokes == nil {
		s.pokes = make(chan struct{}, 1)
	}
	return s.pokes
}

// Run runs a pass every `every` until ctx ends, and a pass for the poked businesses PokeWait after a
// poke.
func (s *Sender) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	s.pokeMu.Lock()
	pokes := s.pokeChan()
	s.pokeMu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.pass(ctx, nil)
		case <-pokes:
			select {
			case <-ctx.Done():
				return
			case <-time.After(PokeWait):
			}
			s.pokeMu.Lock()
			only := s.poked
			s.poked = nil
			s.pokeMu.Unlock()
			if only == nil {
				only = map[string]bool{}
			}
			s.pass(ctx, only)
		}
	}
}

// Pass looks at every business once and returns how many sends an other app took.
func (s *Sender) Pass(ctx context.Context) int { return s.pass(ctx, nil) }

func (s *Sender) pass(ctx context.Context, only map[string]bool) (done int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.businesses(ctx)
	if err != nil {
		s.Log.Printf("sending to other apps: can't list the businesses: %v", err)
		return 0
	}
	for _, t := range list {
		if ctx.Err() != nil {
			break
		}
		if only != nil && !only[t.ID] {
			continue
		}
		n, err := s.Business(ctx, t)
		if err != nil && ctx.Err() == nil {
			s.Log.Printf("sending to other apps %s: %v", t.Slug, err)
		}
		done += n
	}
	return done
}

func (s *Sender) businesses(ctx context.Context) ([]db.Tenant, error) {
	if s.List != nil {
		return s.List(ctx)
	}
	return db.Tenants(ctx, s.DB)
}

// claim is one send held for posting.
type claim struct {
	id, item, body string
	tries          int
	target         send.Target
	lastTry        *time.Time // last_try_at before the claim, put back if the send is released untried
}

// Business does one business's sends: claim, post, record. It returns how many an other app took. A
// panic becomes this business's error, so the others still go.
func (s *Sender) Business(ctx context.Context, t db.Tenant) (done int, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	var claimed []claim
	if err := db.WithTenant(ctx, s.DB, t.ID, func(tx pgx.Tx) error {
		var err error
		claimed, err = s.claim(ctx, tx)
		return err
	}); err != nil {
		return 0, err
	}
	for _, c := range claimed {
		if ctx.Err() != nil {
			return done, ctx.Err() // the held sends go after the hold, with the same id
		}
		// Read the app again just before posting: a Pause, a set-up import or a pause by Seyal (410, too
		// many) since the claim must stop this send, not only the next pass's.
		var why string
		if err := db.WithTenant(ctx, s.DB, t.ID, func(tx pgx.Tx) error {
			var err error
			c.target, why, err = s.recheck(ctx, tx, c)
			return err
		}); err != nil {
			return done, err
		}
		if why != "" {
			s.Log.Printf("sending to other apps %s: %s to %s: held back: %s", t.Slug, c.id, c.target.Name, why)
			continue
		}
		at := s.now()
		v := s.post(ctx, c, at)
		if err := db.WithTenant(ctx, s.DB, t.ID, func(tx pgx.Tx) error {
			return s.record(ctx, tx, c, v, at)
		}); err != nil {
			return done, err
		}
		s.Log.Printf("sending to other apps %s: %s to %s: %s", t.Slug, c.id, c.target.Name, v.Reason)
		if v.Kind == send.Done {
			done++
		}
	}
	return done, nil
}

// claim looks at the business's due sends: it expires, gives up, keeps waiting or claims each (see the
// package's comment).
func (s *Sender) claim(ctx context.Context, tx pgx.Tx) ([]claim, error) {
	if err := ledger.Lock(ctx, tx); err != nil { // first, as every writing transaction does
		return nil, err
	}
	now := s.now()
	rows, err := tx.Query(ctx, `
		SELECT id, item_id::text, target, body, tries, queued_at, coalesce(last_reason, ''), last_try_at FROM sends
		WHERE tenant_id = current_tenant() AND done_at IS NULL AND gave_up_at IS NULL AND expired_at IS NULL
			AND next_at <= $1
		ORDER BY next_at, id LIMIT 100 FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return nil, fmt.Errorf("due sends: %w", err)
	}
	type due struct {
		ID, Item, Target, Body string
		Tries                  int
		QueuedAt               time.Time
		LastReason             string
		LastTry                *time.Time
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[due])
	if err != nil {
		return nil, fmt.Errorf("due sends: %w", err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	all, err := send.Targets(ctx, tx)
	if err != nil {
		return nil, err
	}
	targets := map[string]send.Listed{}
	for _, l := range all {
		targets[l.Name] = l
	}
	lastHour := map[string]int{} // tries made per app in the last hour, counted once per app
	var claimed []claim
	for _, d := range list {
		t, listed := targets[d.Target]
		fail := func(reason string, extra map[string]any) error {
			return s.end(ctx, tx, d.ID, d.Item, d.Target, labelOf(t, d.Target), d.Tries, reason, now, extra)
		}
		switch {
		case !now.Before(d.QueuedAt.Add(ExpireAfter)):
			if err := fail(d.LastReason, map[string]any{"expired": true}); err != nil {
				return nil, err
			}
			continue
		case !listed:
			if err := fail("no other app called "+d.Target, nil); err != nil {
				return nil, err
			}
			continue
		}
		if off := notAllowed(d.Body, t.Target); off != "" {
			if err := fail(t.Name+" may no longer receive "+off, nil); err != nil {
				return nil, err
			}
			continue
		}
		// wait keeps a send that can't go now, with why, and looks at it again after Every. Moving its
		// next_at matters: left as it was, a pile of sends to one paused app would stay first in line on
		// every pass and fill the 100 looked at, so sends to the business's other apps would never be
		// reached. Expiry counts from queued_at, so moving next_at doesn't put it off.
		wait := func(why string) error {
			_, err := tx.Exec(ctx, `UPDATE sends SET last_reason = $2, next_at = $3 WHERE tenant_id = current_tenant() AND id = $1`,
				d.ID, why, now.Add(Every))
			return err
		}
		if why := s.waitWhy(t); why != "" {
			if err := wait(why); err != nil {
				return nil, err
			}
			continue
		}
		if len(claimed) >= Batch {
			continue // the next pass takes it
		}
		n, counted := lastHour[t.Name]
		if !counted {
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM sends WHERE tenant_id = current_tenant() AND target = $1 AND last_try_at > $2`,
				t.Name, now.Add(-time.Hour)).Scan(&n); err != nil {
				return nil, err
			}
		}
		if n >= send.CapPerHour {
			// Too many in an hour: Seyal pauses the app, and admins see why in Leaves Seyal.
			if _, err := send.SeyalPause(ctx, tx, t.Name, send.WhyCap); err != nil {
				return nil, err
			}
			t.State.PausedAt = now
			targets[t.Name] = t
			if err := wait("paused"); err != nil {
				return nil, err
			}
			continue
		}
		lastHour[t.Name] = n + 1
		if _, err := tx.Exec(ctx, `
			UPDATE sends SET tries = tries + 1, next_at = $2, last_try_at = $3
			WHERE tenant_id = current_tenant() AND id = $1`, d.ID, now.Add(Hold), now); err != nil {
			return nil, err
		}
		claimed = append(claimed, claim{id: d.ID, item: d.Item, body: d.Body, tries: d.Tries + 1, target: t.Target,
			lastTry: d.LastTry})
	}
	return claimed, nil
}

// recheck reads c's other app again just before c is posted, and checks what claim checked (except
// expiry and the hourly cap, which the claim already counted). It returns the app as it is now, to post
// with. If the send can no longer go, it releases it untried (its try uncounted, due again now, with
// why), so the next pass ends it or keeps it waiting, and returns why. It only touches a send still
// held by this claim (same tries, not ended).
func (s *Sender) recheck(ctx context.Context, tx pgx.Tx, c claim) (send.Target, string, error) {
	if err := ledger.Lock(ctx, tx); err != nil { // first, as every writing transaction does
		return c.target, "", err
	}
	var held bool
	if err := tx.QueryRow(ctx, `
		SELECT done_at IS NULL AND gave_up_at IS NULL AND expired_at IS NULL AND tries = $2 FROM sends
		WHERE tenant_id = current_tenant() AND id = $1 FOR UPDATE`, c.id, c.tries).Scan(&held); err != nil {
		return c.target, "", fmt.Errorf("recheck a send: %w", err)
	}
	if !held {
		return c.target, "no longer held", nil // ended or changed meanwhile: leave it as it is
	}
	why := ""
	t, err := send.Find(ctx, tx, c.target.Name)
	switch {
	case errors.Is(err, send.ErrNoTarget):
		why = "no other app called " + c.target.Name
	case err != nil:
		return c.target, "", err
	default:
		if off := notAllowed(c.body, t.Target); off != "" {
			why = t.Name + " may no longer receive " + off
		} else {
			why = s.waitWhy(t)
		}
	}
	if why == "" {
		return t.Target, "", nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sends SET tries = tries - 1, next_at = $2, last_try_at = $3, last_reason = $4
		WHERE tenant_id = current_tenant() AND id = $1`, c.id, s.now(), c.lastTry, why); err != nil {
		return c.target, "", fmt.Errorf("release a send: %w", err)
	}
	return c.target, why, nil
}

// waitWhy says why a send to t can't go now, or "".
func (s *Sender) waitWhy(t send.Listed) string {
	switch {
	case !s.On:
		return "sending is switched off on this server"
	case len(t.HostsOff(s.Hosts)) > 0:
		return "its host isn't allowed on this server"
	case t.Paused:
		return "paused in the business's set-up file"
	case t.State.Paused():
		return "paused"
	}
	return ""
}

// notAllowed names a field in body that t may no longer receive (its list in the set-up file changed
// since the send was queued), or "".
func notAllowed(body string, t send.Target) string {
	var b struct {
		Data struct {
			Fields map[string]json.RawMessage `json:"fields"`
			Link   *string                    `json:"link"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &b) != nil {
		return "this body"
	}
	for key := range b.Data.Fields {
		if !slices.Contains(t.Fields, key) {
			return key
		}
	}
	if b.Data.Link != nil && !t.Link {
		return "the item's link"
	}
	return ""
}

func labelOf(t send.Listed, name string) string {
	if t.Label != "" {
		return t.Label
	}
	return name
}

// post signs and posts one claimed send, and judges the answer. It makes a network call, so it runs
// with no transaction open.
func (s *Sender) post(ctx context.Context, c claim, at time.Time) result {
	header, out, err := send.Signed(c.target.Secret, c.id, at, []byte(c.body), send.SigInBody(c.target.Host()))
	if err != nil {
		// A broken secret: only an import fixes it.
		return result{Verdict: send.Verdict{Kind: send.Fix, Reason: send.ReasonFailed}}
	}
	a, err := s.Out.PostRedirect(ctx, c.target.URL, c.target.RedirectTo, header, out)
	r := result{Verdict: send.Judge(a, err, c.target.Expect == send.ExpectOKJSON)}
	if err == nil {
		r.status = a.Status
		if r.Kind == send.Retry {
			r.asked = retryAfter(a.RetryAfter, at)
		}
	}
	return r
}

// result is one try's outcome: the verdict, the reply code (0: no answer) and how long the app asked
// Seyal to wait (Retry-After).
type result struct {
	send.Verdict
	status int
	asked  time.Duration
}

// record keeps one try's result (see the package's comment).
func (s *Sender) record(ctx context.Context, tx pgx.Tx, c claim, r result, at time.Time) error {
	if err := ledger.Lock(ctx, tx); err != nil {
		return err
	}
	var status *int
	if r.status != 0 {
		status = &r.status
	}
	label := c.target.Label
	switch {
	case r.Kind == send.Done:
		if _, err := tx.Exec(ctx, `
			UPDATE sends SET done_at = $2, last_status = $3, last_reason = $4
			WHERE tenant_id = current_tenant() AND id = $1`, c.id, at, status, r.Reason); err != nil {
			return err
		}
		change := map[string]any{"send": c.id, "to": c.target.Name, "label": label, "reason": r.Reason, "tries": c.tries}
		if status != nil {
			change["status"] = *status
		}
		return workitem.RecordSendEvent(ctx, tx, c.item, workitem.KindSendDone, change)
	case r.Kind == send.Retry && c.tries < MaxTries:
		next := at.Add(max(RetryWaits[c.tries-1], r.asked))
		_, err := tx.Exec(ctx, `
			UPDATE sends SET next_at = $2, last_status = $3, last_reason = $4
			WHERE tenant_id = current_tenant() AND id = $1`, c.id, next, status, r.Reason)
		return err
	}
	extra := map[string]any{}
	switch r.Kind {
	case send.Retry:
		extra["gave_up"] = true // after MaxTries
	case send.Gone:
		extra["gone"] = true
		if _, err := send.SeyalPause(ctx, tx, c.target.Name, send.WhyGone); err != nil {
			return err
		}
	default:
		extra["fix"] = true
	}
	if status != nil {
		extra["status"] = *status
		if _, err := tx.Exec(ctx, `UPDATE sends SET last_status = $2 WHERE tenant_id = current_tenant() AND id = $1`,
			c.id, *status); err != nil {
			return err
		}
	}
	return s.end(ctx, tx, c.id, c.item, c.target.Name, label, c.tries, r.Reason, at, extra)
}

// end ends a send that won't go (given up, or expired when extra has "expired"), and records
// item.send_failed with the short reason.
func (s *Sender) end(ctx context.Context, tx pgx.Tx, id, item, target, label string, tries int, reason string, at time.Time,
	extra map[string]any) error {
	column := "gave_up_at"
	if extra["expired"] == true {
		column = "expired_at"
	}
	if _, err := tx.Exec(ctx, `UPDATE sends SET `+column+` = $2, last_reason = nullif($3, '')
		WHERE tenant_id = current_tenant() AND id = $1`, id, at, reason); err != nil {
		return fmt.Errorf("end a send: %w", err)
	}
	change := map[string]any{"send": id, "to": target, "label": label, "reason": reason, "tries": tries}
	for k, v := range extra {
		change[k] = v
	}
	return workitem.RecordSendEvent(ctx, tx, item, workitem.KindSendFailed, change)
}

// retryAfter reads a Retry-After header: a number of seconds, or a date, at most MaxAsked. Anything
// else is 0.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		if n > int(MaxAsked/time.Second) {
			return MaxAsked // checked before multiplying, so it can't overflow
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), MaxAsked)
	}
	return 0
}
