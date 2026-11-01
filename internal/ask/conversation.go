// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/media"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// Reply is what happened after a person wrote something.
type Reply struct {
	Message channel.Message   // what the person wrote, as stored
	Seyal   []channel.Message // what Seyal answered, in order
	Item    workitem.Item     // the work item afterwards
}

// Env is what a conversation uses besides the database.
type Env struct {
	Store media.Store // voice notes and files
	AI    *ai.Setup   // nil, or AI off for "understand": Seyal asks question by question
}

// greetingReply answers a greeting on Tell Seyal without starting anything.
const greetingReply = "Hello! Tell me about the work you want to start, and I'll take it from there."

// Begin handles a message on Tell Seyal. New work starts a work item: the message becomes the first
// entry of its thread, the details it gives are filled in (when AI is on), and Seyal asks for the
// next missing one. A greeting or a question starts nothing; Seyal just replies, and the returned
// Reply has no Item. tx must come from db.WithTenant.
func Begin(ctx context.Context, tx pgx.Tx, env Env, playbookVersionID string, in channel.Incoming) (Reply, error) {
	in.ThreadID = ""
	msg, isNew, err := channel.Receive(ctx, tx, env.Store, in)
	if err != nil {
		return Reply{}, err
	}
	if !isNew && msg.ThreadID != "" { // the channel delivered the same message again
		it, err := workitem.Get(ctx, tx, msg.ThreadID)
		return Reply{Message: msg, Item: it}, err
	}

	c, err := newConversation(ctx, tx, env, playbookVersionID, workitem.Item{}, msg)
	if err != nil {
		return Reply{}, err
	}
	text := strings.TrimSpace(msg.Text)
	pr := c.understand(text, "")
	switch {
	case pr != nil && (pr.Kind == "greeting" || pr.Kind == "question" || pr.Kind == "note") && len(msg.Media) == 0:
		reply := strings.TrimSpace(pr.Reply)
		if reply == "" {
			reply = greetingReply
		}
		return c.replyWithoutItem(limit(reply, 600))
	case pr == nil && isGreeting(text) && len(msg.Media) == 0:
		return c.replyWithoutItem(greetingReply)
	}

	by := ledger.PersonActor(msg.PersonID)
	it, err := workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: playbookVersionID, Via: "chat", Ref: msg.ID, By: by})
	if err != nil {
		return Reply{}, err
	}
	if c.reply.Message, err = channel.FileInto(ctx, tx, msg.ID, it.ID, by); err != nil {
		return Reply{}, err
	}
	c.reply.Item = it
	offered := false
	if pr != nil {
		if _, offered, err = c.apply(pr, text, nil); err != nil {
			return Reply{}, err
		}
	}
	if !offered {
		if err := c.askNext(false); err != nil {
			return Reply{}, err
		}
	}
	return c.reply, nil
}

// replyWithoutItem answers a message that starts nothing. Both stay unfiled (not in any thread).
func (c *conversation) replyWithoutItem(text string) (Reply, error) {
	m, err := channel.Post(c.ctx, c.tx, "", text)
	if err != nil {
		return Reply{}, err
	}
	c.reply.Seyal = append(c.reply.Seyal, m)
	return c.reply, nil
}

// Converse handles a message a person writes in a work item's thread. If Seyal asked whether to add
// a new master item, the message is the yes or no. Otherwise, with AI on, the model reads it (details
// for several fields, a command, a question, a note); without AI, or if AI can't help, an answer to
// Seyal's last question or a short command ("submit", "approve", "reject because ...") is
// recognised, and anything else is a note. Then Seyal asks the next question, if any.
// tx must come from db.WithTenant.
func Converse(ctx context.Context, tx pgx.Tx, env Env, itemID string, in channel.Incoming) (Reply, error) {
	in.ThreadID = itemID
	msg, isNew, err := channel.Receive(ctx, tx, env.Store, in)
	if err != nil {
		return Reply{}, err
	}
	it, err := workitem.Get(ctx, tx, itemID)
	if err != nil || !isNew {
		return Reply{Message: msg, Item: it}, err
	}
	c, err := newConversation(ctx, tx, env, it.PlaybookVersionID, it, msg)
	if err != nil {
		return Reply{}, err
	}

	answered, offered := false, false
	if text := strings.TrimSpace(msg.Text); text != "" {
		f, offer, err := c.pending()
		if err != nil {
			return Reply{}, err
		}
		pendingKey := ""
		if f != nil {
			pendingKey = f.Key
		}
		var pr *proposal
		if offer == "" && !c.isCommand(text) {
			pr = c.understand(text, pendingKey)
		}
		switch {
		case offer != "": // offer is only set when f is
			answered, err = true, c.answerOffer(*f, offer, text)
		case pr != nil:
			answered, offered, err = c.apply(pr, text, f)
		case f != nil:
			answered, err = true, c.answer(*f, text)
		default:
			err = c.command(text)
		}
		if err != nil {
			return Reply{}, err
		}
	}
	if !offered {
		if err := c.askNext(answered); err != nil {
			return Reply{}, err
		}
	}
	return c.reply, nil
}

// Pending returns the field Seyal is waiting for an answer to on this item, or nil.
// tx must come from db.WithTenant.
func Pending(ctx context.Context, tx pgx.Tx, it workitem.Item) (*playbook.Field, error) {
	p, err := workitem.PlaybookFor(ctx, tx, it)
	if err != nil {
		return nil, err
	}
	c := &conversation{ctx: ctx, tx: tx, p: p, reply: Reply{Item: it}}
	f, _, err := c.pending()
	return f, err
}

type conversation struct {
	ctx    context.Context
	tx     pgx.Tx
	env    Env
	p      *playbook.Playbook
	person string
	today  time.Time
	reply  Reply
}

func newConversation(ctx context.Context, tx pgx.Tx, env Env, versionID string, it workitem.Item, msg channel.Message) (*conversation, error) {
	p, err := workitem.PlaybookFor(ctx, tx, workitem.Item{PlaybookVersionID: versionID})
	if err != nil {
		return nil, err
	}
	var zone string
	if err := tx.QueryRow(ctx, "SELECT timezone FROM tenants WHERE id = current_tenant()").Scan(&zone); err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, err
	}
	return &conversation{ctx: ctx, tx: tx, env: env, p: p, person: msg.PersonID, today: time.Now().In(loc),
		reply: Reply{Message: msg, Item: it}}, nil
}

// pending returns the field Seyal last asked about, if it is still empty, and the name it offered to
// add as a new master item, if the question was such an offer.
func (c *conversation) pending() (*playbook.Field, string, error) {
	key, offer, err := c.lastAsked()
	if err != nil || key == "" {
		return nil, "", err
	}
	for i := range c.p.Fields {
		if f := &c.p.Fields[i]; f.Key == key && isEmpty(c.reply.Item.Fields[key]) {
			return f, offer, nil
		}
	}
	return nil, "", nil
}

// lastAsked returns the field of Seyal's last question, and the offered name if it was an offer.
func (c *conversation) lastAsked() (field, offer string, err error) {
	if c.reply.Item.ID == "" {
		return "", "", nil
	}
	var body string
	err = c.tx.QueryRow(c.ctx, `
		SELECT body FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id::text = $1
			AND kind = 'item.asked'
		ORDER BY seq DESC LIMIT 1`, c.reply.Item.ID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var e struct {
		Payload struct {
			Change struct {
				Field string `json:"field"`
				Offer string `json:"offer_new"`
			} `json:"change"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return "", "", err
	}
	return e.Payload.Change.Field, e.Payload.Change.Offer, nil
}

// answer fills field f from the person's answer, or says what was wrong with it.
func (c *conversation) answer(f playbook.Field, text string) error {
	v, err := Understand(f, text, c.today, choices{c.ctx, c.tx})
	if err != nil {
		return c.say(fmt.Sprintf("Sorry, I didn't get that: %s.", strings.TrimSuffix(err.Error(), ".")))
	}
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.SetFields(c.ctx, sp, c.reply.Item.ID, engine.Fields{f.Key: v}, ledger.PersonActor(c.person))
	})
	if err != nil || problem == "" {
		return err
	}
	return c.say("Sorry, I couldn't use that: " + problem + ".")
}

// command runs a short command, if the text is one. Any other text is just a note.
func (c *conversation) command(text string) error {
	it := c.reply.Item
	words := engine.Actions(c.p, engine.Item{State: it.State, Fields: it.Fields})
	waiting := c.p.Approvals != nil && it.State == c.p.Approvals.In
	if waiting {
		words = append(words, engine.Approve, engine.Reject)
	}
	word, reason, ok := Command(text, words)
	switch {
	case !ok:
		return nil
	case waiting && (word == engine.Approve || word == engine.Reject):
		return c.decide(word, reason)
	}
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.Act(c.ctx, sp, it.ID, word, reason, ledger.PersonActor(c.person))
	})
	switch {
	case err != nil:
		return err
	case problem != "":
		return c.say(fmt.Sprintf("I couldn't %s it: %s.", word, problem))
	}
	return c.sayStatus()
}

// decide records the person's approval or refusal for the first waiting slot they can decide.
func (c *conversation) decide(decision, reason string) error {
	rows, _ := c.tx.Query(c.ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL`, c.person)
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	st, err := workitem.Approvals(c.ctx, c.tx, c.reply.Item.ID)
	if err != nil {
		return err
	}
	slot := ""
	for _, s := range st.Slots {
		if slices.Contains(st.Waiting, s.Slot) && slices.Contains(roles, s.Role) {
			slot = s.Slot
			break
		}
	}
	if slot == "" {
		return c.say("This one isn't waiting for a decision from you. It's waiting for: " + humanList(st.Waiting) + ".")
	}
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.Decide(c.ctx, sp, c.reply.Item.ID, c.person, slot, decision, reason)
	})
	switch {
	case err != nil:
		return err
	case strings.Contains(problem, engine.ErrReasonNeeded.Error()):
		return c.say(`Please add a reason, for example "reject because the scope is unclear".`)
	case problem != "":
		return c.say("I couldn't record that: " + problem + ".")
	}
	return c.sayStatus()
}

// sayStatus tells the thread where the item stands after a command.
func (c *conversation) sayStatus() error {
	it := c.reply.Item
	if a := c.p.Approvals; a != nil && it.State == a.In {
		st, err := workitem.Approvals(c.ctx, c.tx, it.ID)
		if err != nil {
			return err
		}
		return c.say("Noted. Waiting for: " + humanList(st.Waiting) + ".")
	}
	if a := c.p.Approvals; a != nil && it.State == a.OnComplete {
		return nil // the playbook's own on-approved actions speak for themselves
	}
	return c.say("Done. It is now " + strings.ReplaceAll(it.State, "_", " ") + ".")
}

// askNext asks for the next empty required field. A question already asked is not repeated unless
// the person just tried to answer it. When everything is filled in after an answer, Seyal says how
// to move on.
func (c *conversation) askNext(answered bool) error {
	it := c.reply.Item
	if engine.IsClosed(c.p, it.State) {
		return nil
	}
	if f := NextField(c.p, it.Fields); f != nil {
		last, _, err := c.lastAsked()
		if err != nil || (last == f.Key && !answered) {
			return err
		}
		m, err := c.post(Question(*f))
		if err != nil {
			return err
		}
		_, err = ledger.Append(c.ctx, c.tx, ledger.Event{
			Kind: "item.asked", SubjectType: "work_item", SubjectID: it.ID,
			Payload: map[string]any{"change": map[string]any{"field": f.Key, "message_id": m.ID}, "item": it},
		}.By(ledger.SystemActor))
		return err
	}
	if !answered {
		return nil
	}
	for _, t := range c.p.Transitions {
		if t.When == "required_filled" && slices.Contains(t.From, it.State) {
			return c.say(fmt.Sprintf("Everything needed is filled in. Say %q when it's ready.", t.Action))
		}
	}
	return nil
}

// try runs a change inside a savepoint. A refusal (wrong value, not allowed now, ...) is undone and
// returned as a plain-word problem for Seyal to explain, without failing the conversation.
func (c *conversation) try(change func(sp pgx.Tx) (workitem.Item, error)) (problem string, err error) {
	sp, err := c.tx.Begin(c.ctx)
	if err != nil {
		return "", err
	}
	it, changeErr := change(sp)
	if changeErr != nil {
		if err := sp.Rollback(c.ctx); err != nil {
			return "", err
		}
		return c.friendly(changeErr), nil
	}
	if err := sp.Commit(c.ctx); err != nil {
		return "", err
	}
	c.reply.Item = it
	return "", nil
}

// friendly rewords errors that mention field keys, so people see the labels instead.
func (c *conversation) friendly(err error) string {
	var missing *engine.MissingError
	if errors.As(err, &missing) {
		labels := make([]string, len(missing.Keys))
		for i, key := range missing.Keys {
			labels[i] = key
			for _, f := range c.p.Fields {
				if f.Key == key {
					labels[i] = Label(f)
				}
			}
		}
		return "still needed: " + strings.Join(labels, ", ")
	}
	return err.Error()
}

func (c *conversation) say(text string) error {
	_, err := c.post(text)
	return err
}

func (c *conversation) post(text string) (channel.Message, error) {
	m, err := channel.Post(c.ctx, c.tx, c.reply.Item.ID, text)
	if err == nil {
		c.reply.Seyal = append(c.reply.Seyal, m)
	}
	return m, err
}

// DBChoices finds people and active master items in the database, for Understand.
// tx must come from db.WithTenant.
func DBChoices(ctx context.Context, tx pgx.Tx) Choices { return choices{ctx, tx} }

// choices finds people and active master items in the database.
type choices struct {
	ctx context.Context
	tx  pgx.Tx
}

func (ch choices) People() ([]Choice, error) {
	rows, _ := ch.tx.Query(ch.ctx, `
		SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() AND status = 'active'`)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Choice])
}

func (ch choices) Items(kind string) ([]Choice, error) {
	rows, _ := ch.tx.Query(ch.ctx, `
		SELECT key, name FROM master_items WHERE tenant_id = current_tenant() AND kind = $1 AND active`, kind)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Choice])
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}
	return false
}

func humanList(keys []string) string {
	if len(keys) == 0 {
		return "nobody"
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strings.ReplaceAll(k, "_", " ")
	}
	return strings.Join(parts, ", ")
}
