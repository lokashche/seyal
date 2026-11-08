// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/send"
)

// Sending to other apps (Phase 4, part 4c-3, D96). A process's `do: send` runs when the item enters
// its state, inside the transaction of the change that brought it there: it freezes the exact body
// (the fields the action names, the item's number, process, state, the business and the send id), keeps
// it in the outbox (the sends table) and records item.send_queued. Nothing is posted here. The sender's
// own loop (internal/outbox) posts it later and records item.send_done or item.send_failed.
//
// Every send event carries the item's unchanged snapshot, with no new version, like item.shared: the
// approvals, the thread, WorkedEscalated and verify keep working, and the New mark, phone notes and
// "the item's latest change" ignore them.

// The ledger kinds of a send.
const (
	KindSendQueued = "item.send_queued" // queued (by Seyal), or queued again (an admin's Send again)
	KindSendDone   = "item.send_done"   // the other app took it
	KindSendFailed = "item.send_failed" // given up, or expired
	KindSendLeft   = "item.send_left"   // an admin's Leave it for a failed send: it won't go
)

// SendKinds are the four kinds: they change nothing on the item.
var SendKinds = []string{KindSendQueued, KindSendDone, KindSendFailed, KindSendLeft}

// MaxSendBody is the most bytes a send's body may have (the sends table allows no more).
const MaxSendBody = 65536

// ReasonTooBig is why a send whose body would be over MaxSendBody ended at once.
const ReasonTooBig = "too long to send (over 64 KB)"

// PublicURL is the server's own address, "https://seyal.example" (SEYAL_PUBLIC_URL), for a send whose
// process and other app both allow the item's link. The sender runs with no web request to take the
// address from. "" leaves every link out. serve sets it once, at start.
var PublicURL string

// AfterSeyal is the "after" of a send queued when Seyal, not a person's decision, moved the item into
// the sending state (a move by: seyal, a schedule's start): the thread says so.
const AfterSeyal = "seyal"

// queueSend freezes and queues one `do: send` for it, which just entered its state (in says how).
func (s session) queueSend(it *Item, a playbook.Action, in arrival) error {
	var slug, currency string
	if err := s.tx.QueryRow(s.ctx, `SELECT slug, currency FROM tenants WHERE id = current_tenant()`).Scan(&slug, &currency); err != nil {
		return fmt.Errorf("workitem: send: business: %w", err)
	}
	label, service, link := a.To, "", false
	switch t, err := send.Find(s.ctx, s.tx, a.To); {
	case errors.Is(err, send.ErrNoTarget):
		// Queued all the same: the sender then gives up at once, in plain words, so the thread shows it.
	case err != nil:
		return err
	default:
		label, service, link = t.Label, t.Service, a.Link && t.Link && PublicURL != ""
	}
	n, err := TakeNumber(s.ctx, s.tx, "send")
	if err != nil {
		return err
	}
	id := "snd_" + slug + "_" + strconv.FormatInt(n, 10)
	at := time.Now().UTC().Truncate(time.Second)
	body, err := s.sendBody(*it, a, slug, currency, id, at, link)
	if err != nil {
		return err
	}
	// A body too long for the outbox (a very long text in a field it sends) must not undo the change
	// that brought the item here: the send is kept with a short stand-in body and ends at once, and the
	// thread says why.
	tooBig := len(body) > MaxSendBody
	if tooBig {
		if body, err = json.Marshal(map[string]any{"id": id, "too_big": true}); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if _, err := s.tx.Exec(s.ctx, `
		INSERT INTO sends (tenant_id, id, item_id, target, body, body_sha256, item_version, fields_sha256, queued_at, next_at,
			gave_up_at, last_reason)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, now(), now(),
			CASE WHEN $8::bool THEN now() END, CASE WHEN $8::bool THEN $9::text END)`,
		id, it.ID, a.To, string(body), hash, it.Version, it.FieldsHash, tooBig, ReasonTooBig); err != nil {
		return fmt.Errorf("workitem: send: queue: %w", err)
	}
	change := map[string]any{"send": id, "to": a.To, "label": label, "service": service, "fields": a.Fields,
		"body": string(body), "body_sha256": hash, "item_version": it.Version, "fields_sha256": it.FieldsHash}
	// After a move by Seyal, not a person: approvals settling (given or refused) is the approvers' decision.
	if in.by.Kind == ledger.System && in.action != "approvals_complete" && in.action != "rejected" {
		change["after"] = AfterSeyal
	}
	if err := s.record(KindSendQueued, *it, change, ledger.SystemActor); err != nil {
		return err
	}
	if !tooBig {
		return nil
	}
	return s.record(KindSendFailed, *it, map[string]any{"send": id, "to": a.To, "label": label, "reason": ReasonTooBig,
		"tries": 0, "too_big": true}, ledger.SystemActor)
}

// sendBody is a send's exact body (design 2.3, "What leaves the server"), in the Standard Webhooks
// shape, with every text guarded against spreadsheet formulas:
//
//	{"type": "<process>.<state>", "id": "snd_…", "timestamp": "…Z", "business": "<slug>",
//	 "data": {"number", "process", "state", "fields": {…}, "row": [number, values…, state, send id]}}
//
// "row" is ready for a sheet: the number, each field's value in the action's order (a list entry or a
// person by name, money in normal units, with the currency once, after the first money field), the
// state and the send id last, so the sheet can skip a repeat.
func (s session) sendBody(it Item, a playbook.Action, slug, currency, id string, at time.Time, link bool) ([]byte, error) {
	fields := map[string]any{}
	row := []any{it.Number}
	currencyShown := false
	for _, key := range a.Fields {
		f := s.field(key)
		if f == nil {
			return nil, fmt.Errorf("workitem: send: unknown field %q", key)
		}
		value, cell, err := s.sendValue(*f, it.Fields[key], currency)
		if err != nil {
			return nil, err
		}
		fields[key] = value
		row = append(row, cell)
		if f.Type == "money" && !currencyShown {
			row, currencyShown = append(row, currency), true
		}
	}
	row = append(row, it.State, id)
	data := map[string]any{"number": it.Number, "process": s.p.ID, "state": it.State, "fields": fields, "row": row}
	if link {
		data["link"] = strings.TrimRight(PublicURL, "/") + "/t/" + slug + "/items/" + it.ID
	}
	body := map[string]any{"type": s.p.ID + "." + it.State, "id": id, "timestamp": at.Format(time.RFC3339),
		"business": slug, "data": data}
	guarded, err := send.GuardAll(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(guarded)
}

// sendValue is one field's value in a send: as data (for "fields") and as a sheet cell (for "row").
// A list entry is {key, name}, a person {name} (never an email), money {minor, currency, text}; numbers
// stay numbers.
func (s session) sendValue(f playbook.Field, v any, currency string) (value, cell any, err error) {
	if v == nil {
		return nil, nil, nil
	}
	switch f.Type {
	case "master", "person":
		name, err := s.display(f, v)
		if err != nil {
			return nil, nil, err
		}
		if f.Type == "person" {
			return map[string]any{"name": name}, name, nil
		}
		return map[string]any{"key": v, "name": name}, name, nil
	case "money":
		x, ok := v.(float64)
		if !ok {
			return nil, nil, fmt.Errorf("workitem: send: %s is not an amount", f.Key)
		}
		minor := int64(math.Round(x))
		major := json.Number(strconv.FormatFloat(float64(minor)/100, 'f', -1, 64))
		return map[string]any{"minor": minor, "currency": currency, "text": Money(minor, currency)}, major, nil
	case "list":
		list, _ := v.([]any)
		names := make([]any, 0, len(list))
		texts := make([]string, 0, len(list))
		for _, x := range list {
			shown, err := s.display(playbook.Field{Key: f.Key, Type: f.Of}, x)
			if err != nil {
				return nil, nil, err
			}
			names, texts = append(names, shown), append(texts, shown)
		}
		return names, strings.Join(texts, ", "), nil
	}
	return v, v, nil
}

// RecordSendEvent records one of the sender's events (KindSendDone or KindSendFailed) about the item,
// by Seyal, with the item's current, unchanged snapshot. It locks the ledger head first, then the item.
// tx must come from db.WithTenant.
func RecordSendEvent(ctx context.Context, tx pgx.Tx, itemID, kind string, change map[string]any) error {
	if kind != KindSendDone && kind != KindSendFailed {
		return errors.New("workitem: not a sender's event")
	}
	it, err := getForUpdate(ctx, tx, itemID)
	if err != nil {
		return err
	}
	return session{ctx: ctx, tx: tx}.record(kind, it, change, ledger.SystemActor)
}

// Send is one send of an item, for its sidebar.
type Send struct {
	ID, Target string
	Label      string // the other app's label now (its name, when the set-up file no longer lists it)
	QueuedAt   time.Time
	Tries      int
	NextAt     time.Time
	LastReason string // a fixed short reason
	Done       bool
	Failed     bool // given up, or expired, and not left: an admin can press Send again or Leave it
	Expired    bool
	Left       bool // it failed, and an admin pressed Leave it: it won't go
}

// Waiting reports whether the send hasn't ended yet.
func (s Send) Waiting() bool { return !s.Done && !s.Failed && !s.Left }

// ItemSends lists the item's sends, oldest first. tx must come from db.WithTenant.
func ItemSends(ctx context.Context, tx pgx.Tx, itemID string) ([]Send, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.target, coalesce(a.label, s.target), s.queued_at, s.tries, s.next_at, coalesce(s.last_reason, ''),
			s.done_at IS NOT NULL, (s.gave_up_at IS NOT NULL OR s.expired_at IS NOT NULL) AND s.left_at IS NULL,
			s.expired_at IS NOT NULL, s.left_at IS NOT NULL
		FROM sends s LEFT JOIN other_apps a ON a.tenant_id = s.tenant_id AND a.name = s.target
		WHERE s.tenant_id = current_tenant() AND s.item_id = $1::uuid
		ORDER BY s.queued_at, s.id`, itemID)
	if err != nil {
		return nil, fmt.Errorf("workitem: sends: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Send])
}

// ErrNotFailed is a Send again or Leave it for a send that hasn't failed or expired (it is done, still
// trying, or already left).
var ErrNotFailed = errors.New("this send hasn't failed, so there is nothing to send again")

// ErrCantGoAgain is a Send again that can't help: the send was too long to go, or the set-up file no
// longer lists its other app. Leave it is the way to clear it.
var ErrCantGoAgain = errors.New("this send can't go again")

// lockFailed locks one of the item's sends for Send again or Leave it, and checks that it failed or
// expired and wasn't left. It returns the send's other app, body hash and last reason.
func lockFailed(ctx context.Context, tx pgx.Tx, it Item, sendID string) (target, hash, reason string, err error) {
	var failed bool
	err = tx.QueryRow(ctx, `
		SELECT target, body_sha256, coalesce(last_reason, ''),
			(gave_up_at IS NOT NULL OR expired_at IS NOT NULL) AND left_at IS NULL FROM sends
		WHERE tenant_id = current_tenant() AND id = $1 AND item_id = $2::uuid FOR UPDATE`, sendID, it.ID).
		Scan(&target, &hash, &reason, &failed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", "", ErrNotFound
	case err != nil:
		return "", "", "", fmt.Errorf("workitem: send: %w", err)
	case !failed:
		return "", "", "", ErrNotFailed
	}
	return target, hash, reason, nil
}

// SendAgain queues a failed or expired send again, as the admin by (the item's [Send again]): the same
// send id and the same frozen body, with its tries and its 24 hours starting afresh. It is an
// item.send_queued event by the admin, with "again": true. tx must come from db.WithTenant; the caller
// checks that by is an admin.
func SendAgain(ctx context.Context, tx pgx.Tx, itemID, sendID string, by ledger.Actor) error {
	it, err := getForUpdate(ctx, tx, itemID)
	if err != nil {
		return err
	}
	target, hash, reason, err := lockFailed(ctx, tx, it, sendID)
	if err != nil {
		return err
	}
	if reason == ReasonTooBig {
		return ErrCantGoAgain
	}
	label := target
	switch t, err := send.Find(ctx, tx, target); {
	case errors.Is(err, send.ErrNoTarget):
		return ErrCantGoAgain // it would only fail again at once
	case err != nil:
		return err
	default:
		label = t.Label
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sends SET queued_at = now(), next_at = now(), tries = 0, last_try_at = NULL, last_status = NULL,
			last_reason = NULL, gave_up_at = NULL, expired_at = NULL
		WHERE tenant_id = current_tenant() AND id = $1`, sendID); err != nil {
		return fmt.Errorf("workitem: send again: %w", err)
	}
	return session{ctx: ctx, tx: tx}.record(KindSendQueued, it,
		map[string]any{"send": sendID, "to": target, "label": label, "body_sha256": hash, "again": true}, by)
}

// LeaveSend is an admin's Leave it for a failed or expired send (the item's [Leave it]): the send won't
// go, and it no longer counts as failed in My Work or in Leaves Seyal. It is an item.send_left event by
// the admin. tx must come from db.WithTenant; the caller checks that by is an admin.
func LeaveSend(ctx context.Context, tx pgx.Tx, itemID, sendID string, by ledger.Actor) error {
	it, err := getForUpdate(ctx, tx, itemID)
	if err != nil {
		return err
	}
	target, _, _, err := lockFailed(ctx, tx, it, sendID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sends SET left_at = now() WHERE tenant_id = current_tenant() AND id = $1`,
		sendID); err != nil {
		return fmt.Errorf("workitem: leave a send: %w", err)
	}
	label := target
	if t, err := send.Find(ctx, tx, target); err == nil {
		label = t.Label
	}
	return session{ctx: ctx, tx: tx}.record(KindSendLeft, it, map[string]any{"send": sendID, "to": target, "label": label}, by)
}

// SendPlan is a `do: send` that may still run for an item: the "will send" line of its sidebar.
type SendPlan struct {
	State   string // the state whose entry sends, e.g. "approved"
	To      string // the other app's name
	Label   string // its label ("" when the set-up file lists no such app)
	Service string // who runs it, e.g. "Google"
	Line    string // what will go, as people read it: "J-2026-0007 · Sundaram Textiles · ₹15,000.00"
	Link    bool   // the item's link (the server's address and the item's id) goes too
}

// SendPlans lists the sends that would run if the item moved on: each `do: send` of a state the item
// can still reach from where it is now (not the state it is in: that one has run). A closed item has
// none. Each line shows the values as the send would freeze them: a `do: copy` listed before the send in
// the same state runs first, so its value is shown. tx must come from db.WithTenant.
func SendPlans(ctx context.Context, tx pgx.Tx, it Item) ([]SendPlan, error) {
	s, err := open(ctx, tx, it.PlaybookVersionID)
	if err != nil {
		return nil, err
	}
	if len(send.Sends(s.p)) == 0 || engine.IsClosed(s.p, it.State) {
		return nil, nil
	}
	var currency string
	if err := tx.QueryRow(ctx, `SELECT currency FROM tenants WHERE id = current_tenant()`).Scan(&currency); err != nil {
		return nil, err
	}
	ahead := Reachable(s.p, it.State)
	var plans []SendPlan
	for _, state := range slices.Sorted(maps.Keys(s.p.On)) { // the order of send.Sends
		if !ahead[state] {
			continue
		}
		fields := maps.Clone(it.Fields) // as enter() leaves them when it reaches each send
		if fields == nil {
			fields = engine.Fields{}
		}
		for _, a := range s.p.On[state] {
			switch a.Do {
			case "copy":
				if a.IfEmpty && !engine.IsEmpty(fields[a.To]) {
					continue
				}
				if v := fields[a.From]; v == nil {
					delete(fields, a.To)
				} else {
					fields[a.To] = v
				}
				continue
			case "send":
			default:
				continue
			}
			plan := SendPlan{State: state, To: a.To}
			if t, err := send.Find(ctx, tx, a.To); err == nil {
				plan.Label, plan.Service = t.Label, t.Service
				plan.Link = a.Link && t.Link && PublicURL != "" // as queueSend decides it
			} else if !errors.Is(err, send.ErrNoTarget) {
				return nil, err
			}
			parts := []string{it.Number}
			for _, key := range a.Fields {
				f := s.field(key)
				if f == nil {
					continue
				}
				shown := "(empty)"
				if v := fields[key]; v != nil {
					if x, ok := v.(float64); ok && f.Type == "money" {
						shown = Money(int64(math.Round(x)), currency)
					} else if shown, err = s.display(*f, v); err != nil {
						return nil, err
					}
				}
				parts = append(parts, shown)
			}
			plan.Line = strings.Join(parts, " · ")
			plans = append(plans, plan)
		}
	}
	return plans, nil
}

// Reachable lists the states an item in state from can still get to, by any move or by approvals
// settling (given or refused). from itself is in it only when a way leads back to it.
func Reachable(p *playbook.Playbook, from string) map[string]bool {
	next := map[string][]string{}
	for _, t := range p.Transitions {
		for _, f := range t.From {
			next[f] = append(next[f], t.To)
		}
	}
	if a := p.Approvals; a != nil {
		next[a.In] = append(next[a.In], a.OnComplete, a.OnReject)
	}
	seen := map[string]bool{}
	todo := slices.Clone(next[from])
	for len(todo) > 0 {
		st := todo[0]
		todo = todo[1:]
		if st == "" || seen[st] {
			continue
		}
		seen[st] = true
		todo = append(todo, next[st]...)
	}
	return seen
}
