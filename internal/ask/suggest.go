// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Suggestions with Yes and No (D68, M8; build part 6; D74). Anything that moves work and did not come
// from exact typed words only becomes a suggestion: a move or hand-over in a voice note, a move the AI
// model read, a typed hand-over that isn't the whole command, and an AI or voice value for a person
// field that says who gets the work or who may take a move. Seyal asks, for example, "Pass J-2026-0007
// to Dev Example, with the note "he knows them"?" with [Yes, pass it] [No]. Nothing moves before Yes.
//
// The suggestion is kept in the ledger as an item.asked event with "propose" (no new event kind, no
// new table). Yes works only while all four of these hold ("When does Yes still work?"):
//  1. the suggestion is for this item, and the person tapping is the one it was made for;
//  2. no event has used it yet (named it as its "suggestion"), so a Yes never works twice;
//  3. that person hasn't written in the thread since: their own next message drops it (other people's
//     messages don't, nor something the person shared from another app, D93);
//  4. the item hasn't changed since: its latest change is still the one the suggestion recorded.
// On Yes, the change runs as the person (human), with every check again, and each event names the
// suggestion (workitem.Confirming).

// propose is what a suggestion would do, as kept in its item.asked event.
type propose struct {
	Do     string        `json:"do"`               // doPass, doTake, doMove, doSendBack or doFill
	To     string        `json:"to,omitempty"`     // doPass: "person:<id>" or "team:<role>"; doMove, doSendBack: the move
	Note   string        `json:"note,omitempty"`   // the note, or the reason for the move
	Fields engine.Fields `json:"fields,omitempty"` // doFill: the person fields and who they would name
	Via    string        `json:"via,omitempty"`    // chat or voice: where the words came from
	For    string        `json:"for"`              // the person who asked; only they may confirm it
	// ItemSeq is the ledger number of the item's latest change when Seyal asked. Any change since
	// makes the suggestion stale.
	ItemSeq int64 `json:"item_seq"`
}

const (
	doMove = "move" // a move of the process ("submit", "cancel because …")
	doFill = "fill" // person fields that carry power (engine.PowerFields)
)

// suggestion is a propose read back from the ledger, with its own ledger number and the message Seyal
// asked it in.
type suggestion struct {
	Seq       int64
	Propose   propose
	MessageID string
}

// Seyal's words around suggestions.
const (
	// suggestionSmall goes after the question, in the thread everyone sees, so it names the person
	// rather than saying "you" (%s is their name).
	suggestionSmall = "Only %s can answer this. It stays in the record either way."
	changedSince    = "Things changed since; say it again."
	leftAsItIs      = "Fine, I left it as it is."
	spokenYes       = "To say yes to this, tap Yes or type it."
)

// act does what typed words ask at once, or, for words from a voice note or read by the AI model,
// suggests it first.
func (c *conversation) act(pr propose) error {
	if c.spoken || c.askFirst {
		return c.suggest(pr)
	}
	if pr.Via == "" {
		pr.Via = "chat"
	}
	return c.carry(pr)
}

// suggest asks the person to confirm pr with Yes. It first tries pr and undoes it at once (see dryRun),
// so something that would be refused is explained now and never suggested ("Chitra isn't in Ops …").
func (c *conversation) suggest(pr propose) error {
	if pr.Via == "" {
		pr.Via = "chat"
		if c.spoken {
			pr.Via = "voice"
		}
	}
	if pr.Do == doFill {
		// Each field on its own: one that would be refused is explained, the others are still suggested.
		ok := engine.Fields{}
		var problems []string
		for _, key := range slices.Sorted(maps.Keys(pr.Fields)) {
			one := pr
			one.Fields = engine.Fields{key: pr.Fields[key]}
			problem, err := c.dryRun(one)
			if err != nil {
				return err
			}
			if problem != "" {
				problems = append(problems, problem)
				continue
			}
			ok[key] = pr.Fields[key]
		}
		if len(problems) > 0 {
			if err := c.say(c.refused(pr, strings.Join(problems, "; "))); err != nil {
				return err
			}
		}
		if len(ok) == 0 {
			return nil
		}
		pr.Fields = ok
	} else {
		problem, err := c.dryRun(pr)
		if err != nil {
			return err
		}
		if problem != "" {
			return c.say(c.refused(pr, problem))
		}
	}
	k, err := c.kin()
	if err != nil {
		return err
	}
	text, err := c.question(pr, k)
	if err != nil {
		return err
	}
	if pr.ItemSeq, err = latestChange(c.ctx, c.tx, c.reply.Item.ID); err != nil {
		return err
	}
	pr.For = c.person
	m, err := c.post(text + " " + fmt.Sprintf(suggestionSmall, k.name(c.person)))
	if err != nil {
		return err
	}
	_, err = ledger.Append(c.ctx, c.tx, ledger.Event{
		Kind: "item.asked", SubjectType: "work_item", SubjectID: c.reply.Item.ID,
		Payload: map[string]any{"change": map[string]any{"propose": pr, "message_id": m.ID}, "item": c.reply.Item},
	}.By(ledger.SystemActor))
	c.suggested = true
	return err
}

// dryRun tries pr inside a savepoint and always rolls it back, so nothing of it is kept: no event, no
// message, no change to the item. It returns why pr would be refused, or "".
func (c *conversation) dryRun(pr propose) (problem string, err error) {
	sp, err := c.tx.Begin(c.ctx)
	if err != nil {
		return "", err
	}
	tx, item := c.tx, c.reply.Item
	c.tx = sp
	problem, _, err = c.perform(pr)
	c.tx, c.reply.Item = tx, item
	if rerr := sp.Rollback(c.ctx); rerr != nil && err == nil {
		err = rerr
	}
	return problem, err
}

// perform does what pr says, as the person, with the same checks as the side panel. A refusal comes
// back as problem; notice is a warning that didn't stop a pass on.
func (c *conversation) perform(pr propose) (problem, notice string, err error) {
	by := ledger.PersonActor(c.person)
	id := c.reply.Item.ID
	switch pr.Do {
	case doTake:
		problem, err = c.try(func(sp pgx.Tx) (workitem.Item, error) {
			return workitem.Take(c.ctx, sp, id, "", pr.Via, by)
		})
	case doPass:
		to := workitem.PassOn{Note: pr.Note, Via: pr.Via}
		if kind, key, _ := strings.Cut(pr.To, ":"); kind == "team" {
			to.Team = key
		} else {
			to.Person = key
		}
		problem, err = c.try(func(sp pgx.Tx) (workitem.Item, error) {
			changed, n, err := workitem.Pass(c.ctx, sp, id, to, by)
			notice = n
			return changed, err
		})
	case doMove, doSendBack:
		problem, err = c.try(func(sp pgx.Tx) (workitem.Item, error) {
			return workitem.Act(c.ctx, sp, id, pr.To, pr.Note, by)
		})
	case doFill:
		var problems []string
		problems, err = c.setFields(pr.Fields, by)
		problem = strings.Join(problems, "; ")
	default:
		problem = "I don't know how to do that"
	}
	return problem, notice, err
}

// carry does pr now and says how it went.
func (c *conversation) carry(pr propose) error {
	problem, notice, err := c.perform(pr)
	switch {
	case err != nil:
		return err
	case problem != "":
		return c.say(c.refused(pr, problem))
	}
	switch pr.Do {
	case doTake:
		return c.say("It's yours now.")
	case doPass:
		if strings.HasPrefix(notice, "Passed to") { // an admin passing outside the step's list says so itself
			return c.say(notice)
		}
		k, err := c.kin()
		if err != nil {
			return err
		}
		return c.say(strings.TrimSpace(fmt.Sprintf("Passed to %s. %s", k.holderName(c.reply.Item), notice)))
	case doSendBack:
		k, err := c.kin()
		if err != nil {
			return err
		}
		return c.say(fmt.Sprintf("Sent back to %s (%s).", k.holderName(c.reply.Item), human(c.reply.Item.State)))
	case doMove:
		return c.sayStatus()
	}
	return c.say("Saved.")
}

// refused says why pr can't be done.
func (c *conversation) refused(pr propose, problem string) string {
	switch pr.Do {
	case doSendBack:
		return fmt.Sprintf("I couldn't send it back: %s.", strings.TrimSuffix(problem, "."))
	case doMove:
		return fmt.Sprintf("I couldn't %s it: %s.", pr.To, problem)
	case doFill:
		return "I couldn't use all of that: " + strings.TrimSuffix(problem, ".") + "."
	}
	return sentenceOf(problem)
}

// question is Seyal's question for a suggestion: "Pass J-2026-0007 to Dev Example, with the note "…"?".
func (c *conversation) question(pr propose, k kin) (string, error) {
	it := c.reply.Item
	switch pr.Do {
	case doTake:
		if it.Team != "" {
			return fmt.Sprintf("Take %s from %s?", it.Number, k.holderName(it)), nil
		}
		return fmt.Sprintf("Take %s?", it.Number), nil
	case doPass:
		kind, key, _ := strings.Cut(pr.To, ":")
		to := k.name(key)
		if kind == "team" {
			to = workitem.TeamName(key, k.roleNames)
		}
		return fmt.Sprintf("Pass %s to %s%s?", it.Number, to, quoted(", with the note", pr.Note)), nil
	case doSendBack:
		backs, err := workitem.BackMoves(c.ctx, c.tx, it, k.actor)
		if err != nil {
			return "", err
		}
		where := human(pr.To)
		if i := slices.IndexFunc(backs, func(b workitem.BackMove) bool { return b.Action == pr.To }); i >= 0 {
			where = k.backLabel(backs[i])
		}
		return fmt.Sprintf("Send %s back to %s%s?", it.Number, where, quoted(", with the reason", pr.Note)), nil
	case doMove:
		return fmt.Sprintf("Do %q on %s%s?", human(pr.To), it.Number, quoted(", with the reason", pr.Note)), nil
	}
	var parts []string
	for _, key := range slices.Sorted(maps.Keys(pr.Fields)) {
		label := key
		f := c.field(key)
		if f != nil {
			label = Label(*f)
		}
		if f != nil && f.Type != "person" { // an answer from shared words (D93): the value itself
			parts = append(parts, fmt.Sprintf("%q to %q", label, c.plain(*f, pr.Fields[key])))
			continue
		}
		who := "nobody"
		if id, _ := pr.Fields[key].(string); id != "" {
			who = k.name(id)
		}
		parts = append(parts, fmt.Sprintf("%q to %s", label, who))
	}
	return fmt.Sprintf("Set %s on %s?", strings.Join(parts, " and "), it.Number), nil
}

// plain writes a value of field f as people read it, for a suggestion's question: a list entry by its
// name, money in normal units, a list joined with commas.
func (c *conversation) plain(f playbook.Field, v any) string {
	switch x := v.(type) {
	case []any:
		parts := make([]string, len(x))
		for i, one := range x {
			parts[i] = c.plain(playbook.Field{Key: f.Key, Type: f.Of, Kind: f.Kind}, one)
		}
		return strings.Join(parts, ", ")
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		if f.Type == "money" {
			return strconv.FormatFloat(x/100, 'f', 2, 64)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		if f.Type == "master" {
			var name string
			if err := c.tx.QueryRow(c.ctx, `SELECT name FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND key = $2`,
				f.Kind, x).Scan(&name); err == nil {
				return name
			}
		}
		return x
	}
	return fmt.Sprint(v)
}

// quoted is `, with the note "…"`, or "" when there is no note.
func quoted(lead, note string) string {
	if note == "" {
		return ""
	}
	return fmt.Sprintf("%s %q", lead, note)
}

// yesLabel is the words on a suggestion's Yes button.
func yesLabel(do string) string {
	switch do {
	case doTake:
		return "Yes, take it"
	case doPass:
		return "Yes, pass it"
	case doSendBack:
		return "Yes, send it back"
	case doFill:
		return "Yes, set it"
	}
	return "Yes, do it"
}

// buttons are a suggestion's Yes and No. They send "yes" and "no" as the person's message, and say which
// suggestion they answer, so a Yes from an old page can't confirm a newer suggestion.
func (s suggestion) buttons() []Quick {
	b := "suggestion:" + strconv.FormatInt(s.Seq, 10)
	return []Quick{{Label: yesLabel(s.Propose.Do), Text: "yes", Button: b}, {Label: "No", Text: "no", Button: b}}
}

// confirm handles a Yes or No to Seyal's suggestion. handled is false when the message isn't one; it is
// then read as usual. busy means Seyal is also waiting for another answer (a field question, an offer, a
// "which one?"), so typed words may be meant for that.
//
// Typed words count only when they are a clear "yes" or "no" (confirmWord): "1", "0" or "ok" may be an
// answer to a field question. They count for someone else's suggestion only when Seyal waits for no other
// answer, and for the person's own suggestion only when it is Seyal's newest question. A Yes from a voice
// note doesn't confirm: Seyal asks again, for a tap or typed words (D74).
func (c *conversation) confirm(text string, busy bool) (handled bool, err error) {
	yes, ok := confirmWord(text)
	if !ok {
		return false, nil
	}
	all, err := suggestions(c.ctx, c.tx, c.reply.Item.ID)
	if err != nil {
		return false, err
	}
	var s *suggestion
	switch {
	case c.button != 0:
		i := slices.IndexFunc(all, func(s suggestion) bool { return s.Seq == c.button })
		if i < 0 {
			return true, c.say(changedSince)
		}
		s = &all[i]
	case len(all) == 0:
		return false, nil
	default:
		if i := slices.IndexFunc(all, func(s suggestion) bool { return s.Propose.For == c.person }); i >= 0 {
			s = &all[i]
		}
	}
	if s == nil || s.Propose.For != c.person {
		// Only the person a suggestion is for may confirm it (D68, decision 16).
		other := s
		if other == nil {
			other = &all[0]
		}
		if c.button == 0 && busy {
			return false, nil
		}
		why, err := whyNot(c.ctx, c.tx, c.reply.Item.ID, *other, c.reply.Message.ID)
		if err != nil || (why != "" && c.button == 0) {
			return false, err
		}
		var name string
		if err := c.tx.QueryRow(c.ctx, `SELECT display_name FROM persons WHERE tenant_id = current_tenant() AND id::text = $1`,
			other.Propose.For).Scan(&name); err != nil {
			return false, err
		}
		return true, c.say(fmt.Sprintf("Only %s can confirm this.", name))
	}
	why, err := whyNot(c.ctx, c.tx, c.reply.Item.ID, *s, c.reply.Message.ID)
	if err != nil {
		return false, err
	}
	if c.button == 0 {
		later := false
		if busy {
			if later, err = askedAfter(c.ctx, c.tx, c.reply.Item.ID, s.Seq); err != nil {
				return false, err
			}
		}
		switch {
		case why == wroteSince:
			return false, nil // their own message dropped it; this one is read as usual
		case busy && (why != "" || later):
			return false, nil // Seyal's other question is the one still open, or the newer one
		}
	}
	switch {
	case !yes:
		return true, c.say(leftAsItIs)
	case why != "":
		return true, c.say(changedSince)
	case c.spoken:
		// A Yes from a voice note: Seyal asks again, so the Yes is a tap or typed (moving-work design,
		// decision 15). This voice note dropped the old suggestion, so the new one takes its place.
		if err := c.say(spokenYes); err != nil {
			return true, err
		}
		return true, c.suggest(s.Propose)
	}
	saved := c.ctx
	c.ctx = workitem.Confirming(c.ctx, s.Seq)
	err = c.carry(s.Propose)
	c.ctx = saved
	return true, err
}

// confirmWord reads a typed Yes or No to a suggestion: exactly "yes" or "no", in any case, with spaces
// around it and one full stop at the end allowed ("Yes.", " no "). ok is false for anything else, also
// for "ok", "y", "yeah", "1" and "0": they may be meant for a field question, and a suggestion moves
// work (D74). Other yes/no questions keep their wider list (parseYesNo).
func confirmWord(text string) (yes, ok bool) {
	word := strings.TrimSpace(text)
	word = strings.TrimSpace(strings.TrimSuffix(word, "."))
	switch strings.ToLower(word) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// askedAfter reports whether Seyal asked the item another question (a field, an offer, a "which one?")
// after the suggestion with ledger number seq.
func askedAfter(ctx context.Context, tx pgx.Tx, itemID string, seq int64) (bool, error) {
	var later bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ledger
			WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
				AND kind = 'item.asked' AND seq > $2 AND body::jsonb->'payload'->'change'->'propose' IS NULL)`,
		itemID, seq).Scan(&later); err != nil {
		return false, fmt.Errorf("ask: asked after: %w", err)
	}
	return later, nil
}

// Why a suggestion no longer works (whyNot).
const (
	wroteSince  = "wrote since"  // its person wrote in the thread after it
	usedAlready = "used already" // an event already names it
	itemChanged = "item changed" // the item changed after it
)

// whyNot says why Yes on s would not work now, or "" when it would. except is the message being read
// now (the Yes itself), which doesn't count as writing since.
func whyNot(ctx context.Context, tx pgx.Tx, itemID string, s suggestion, except string) (string, error) {
	since, err := wrote(ctx, tx, itemID, s.Propose.For, s.MessageID, except)
	if err != nil || since {
		return wroteSince, err
	}
	var used bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ledger
			WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
				AND kind <> 'item.asked' AND body::jsonb->'payload'->'change'->>'suggestion' = $2)`,
		itemID, strconv.FormatInt(s.Seq, 10)).Scan(&used); err != nil {
		return "", fmt.Errorf("ask: suggestion used: %w", err)
	}
	if used {
		return usedAlready, nil
	}
	latest, err := latestChange(ctx, tx, itemID)
	if err != nil {
		return "", err
	}
	if latest != s.Propose.ItemSeq {
		return itemChanged, nil
	}
	return "", nil
}

// latestChange returns the ledger number of the item's latest change: its newest event that isn't one
// of Seyal's questions, or a share (item.shared, D91), which changes nothing on the item.
func latestChange(ctx context.Context, tx pgx.Tx, itemID string) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(max(seq), 0) FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
			AND kind NOT IN ('item.asked', 'item.shared')`, itemID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("ask: latest change: %w", err)
	}
	return seq, nil
}

// suggestions returns the item's suggestions, newest first.
func suggestions(ctx context.Context, tx pgx.Tx, itemID string) ([]suggestion, error) {
	rows, _ := tx.Query(ctx, `
		SELECT seq, body::jsonb->'payload'->'change' FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
			AND kind = 'item.asked' AND body::jsonb->'payload'->'change'->'propose' IS NOT NULL
		ORDER BY seq DESC`, itemID)
	type row struct {
		Seq    int64
		Change struct {
			Propose   propose `json:"propose"`
			MessageID string  `json:"message_id"`
		}
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, fmt.Errorf("ask: suggestions: %w", err)
	}
	out := make([]suggestion, len(got))
	for i, r := range got {
		out[i] = suggestion{Seq: r.Seq, Propose: r.Change.Propose, MessageID: r.Change.MessageID}
	}
	return out, nil
}

// openSuggestion returns person's newest suggestion on the item while Yes on it would still work, or nil.
func openSuggestion(ctx context.Context, tx pgx.Tx, itemID, person string) (*suggestion, error) {
	all, err := suggestions(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(all, func(s suggestion) bool { return s.Propose.For == person })
	if i < 0 {
		return nil, nil
	}
	why, err := whyNot(ctx, tx, itemID, all[i], "")
	if err != nil || why != "" {
		return nil, err
	}
	return &all[i], nil
}

// wrote reports whether person wrote in the item's thread after the message with id after, not
// counting the message with id except.
func wrote(ctx context.Context, tx pgx.Tx, itemID, person, after, except string) (bool, error) {
	var since bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM messages m JOIN messages q ON q.tenant_id = m.tenant_id AND q.id::text = $3
			WHERE m.tenant_id = current_tenant() AND m.work_item_id::text = $1 AND m.author_kind = 'person'
				AND m.author_person_id::text = $2 AND m.sent_at > q.sent_at AND m.id::text <> $4
				AND m.via IS NULL)`, // something they shared isn't their own words (D93)
		itemID, person, after, except).Scan(&since); err != nil {
		return false, fmt.Errorf("ask: written since: %w", err)
	}
	return since, nil
}

// flush asks the suggestion kept for later (an AI value for a person field that carries power), once
// everything else from the message is saved, so that saving doesn't make it stale at once.
func (c *conversation) flush() error {
	if c.later == nil {
		return nil
	}
	pr := *c.later
	c.later = nil
	return c.suggest(pr)
}

// tookFrom returns the team the person took the item from, when their latest hand-over of this item was
// a take from a team, or "".
func (c *conversation) tookFrom() (string, error) {
	var how, team string
	err := c.tx.QueryRow(c.ctx, `
		SELECT coalesce(body::jsonb->'payload'->'change'->>'how', ''), coalesce(body::jsonb->'payload'->'change'->>'from_team', '')
		FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
			AND kind = 'item.assigned' AND body::jsonb->'payload'->'change'->>'assignee_id' = $2
		ORDER BY seq DESC LIMIT 1`, c.reply.Item.ID, c.person).Scan(&how, &team)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("ask: taken from: %w", err)
	case how == workitem.HowTake:
		return team, nil
	}
	return "", nil
}
