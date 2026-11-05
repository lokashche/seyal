// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"cmp"
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
	Answer  *Answer           // on Tell Seyal: the answer to a question about the work (D67); nothing starts
	Asked   *Asked            // on Tell Seyal: a question to answer with AnswerAsked, in a new transaction
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
// Reply has no Item. A question about the work comes back as Asked, for the caller to answer after
// this transaction (see Asked). tx must come from db.WithTenant.
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
	// A question about the work gets an answer from the data (D67), after this transaction. When the
	// model couldn't read the message (AI off, or failing just now), only a clear question counts:
	// Seyal says it can't answer now, starts nothing, and offers to start it as work.
	switch {
	case !wordsOnly(msg):
	case pr != nil && pr.Kind == "question":
		c.reply.Asked = &Asked{MessageID: msg.ID, PersonID: msg.PersonID, Question: text, Reply: pr.Reply}
		return c.reply, nil
	case pr == nil && looksLikeQuestion(text):
		c.reply.Answer = Unread(env, msg.ID, text, "")
		return c.reply, nil
	}
	switch {
	case pr != nil && (pr.Kind == "greeting" || pr.Kind == "note") && wordsOnly(msg):
		reply := strings.TrimSpace(pr.Reply)
		if reply == "" {
			reply = greetingReply
		}
		return c.replyWithoutItem(limit(reply, 600))
	case pr == nil && isGreeting(text) && wordsOnly(msg):
		return c.replyWithoutItem(greetingReply)
	}

	return c.start(pr)
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
		command := c.isCommand(text)
		if offer == nil && !command {
			pr = c.understand(text, pendingKey)
		}
		switch {
		case command: // a command wins over a pending question or offer ("submit" isn't an amount)
			err = c.command(text)
		case offer != nil: // offer is only set when f is
			answered = true
			offered, err = c.answerOffer(*f, *offer, text)
		case pr != nil:
			answered, offered, err = c.apply(pr, text, f)
		case f != nil:
			answered = true
			offered, err = c.answer(*f, text)
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
	// A command may have started linked work or moved a linked item on; ask there too.
	if err := nudgeLinked(ctx, tx, env, it.ID); err != nil {
		return Reply{}, err
	}
	return c.reply, nil
}

// nudgeLinked asks the next question in the items linked to this one (not in this one: its own
// conversation has just done that).
func nudgeLinked(ctx context.Context, tx pgx.Tx, env Env, itemID string) error {
	rows, _ := tx.Query(ctx, `
		SELECT CASE WHEN to_item_id::text = $1 THEN from_item_id::text ELSE to_item_id::text END
		FROM work_item_links
		WHERE tenant_id = current_tenant() AND kind = 'parent' AND (from_item_id::text = $1 OR to_item_id::text = $1)`, itemID)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return askIn(ctx, tx, env, ids)
}

// askIn asks the next question, if any, in each item (none in closed ones; none repeated).
func askIn(ctx context.Context, tx pgx.Tx, env Env, ids []string) error {
	for _, id := range ids {
		it, err := workitem.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		c, err := newConversation(ctx, tx, env, it.PlaybookVersionID, it, channel.Message{})
		if err != nil {
			return err
		}
		if err := c.askNext(false); err != nil {
			return err
		}
	}
	return nil
}

// Pending returns the field Seyal is waiting for an answer to on this item, or nil, and the name it
// offered to add as a new entry if the question is that yes/no offer. tx must come from db.WithTenant.
func Pending(ctx context.Context, tx pgx.Tx, it workitem.Item) (*playbook.Field, string, error) {
	p, err := workitem.PlaybookFor(ctx, tx, it)
	if err != nil {
		return nil, "", err
	}
	c := &conversation{ctx: ctx, tx: tx, p: p, reply: Reply{Item: it}}
	f, of, err := c.pending()
	if of == nil {
		return f, "", err
	}
	return f, of.Name, err
}

// offer is Seyal's open question "add <Name> as a new entry?", with further new names queued after it.
type offer struct {
	Name string    `json:"offer_new"`
	Then []newItem `json:"then"`
}

type conversation struct {
	ctx       context.Context
	tx        pgx.Tx
	env       Env
	versionID string // the process version the conversation follows
	p         *playbook.Playbook
	person    string
	today     time.Time
	reply     Reply
	// spoken: the message's words came from a voice note (it has a transcript, which the server
	// checked). Such words never run a command (see voiceGuard).
	spoken bool
}

// voiceGuard is Seyal's reply when words from a voice note read as a command (move, cancel,
// approve, ...). Speech services mishear, names most of all, so a spoken command never runs at once;
// the person types it or taps the button (moving-work design, decision 15). A spoken answer to
// Seyal's question is still used.
const voiceGuard = "To do that, type it or tap the button."

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
	return &conversation{ctx: ctx, tx: tx, env: env, versionID: versionID, p: p, person: msg.PersonID, today: time.Now().In(loc),
		reply: Reply{Message: msg, Item: it}, spoken: msg.Transcript != nil}, nil
}

// pending returns the field Seyal last asked about, if it is still empty, and the offer to add a new
// master item, if the question was such an offer.
func (c *conversation) pending() (*playbook.Field, *offer, error) {
	key, of, err := c.lastAsked()
	if err != nil || key == "" {
		return nil, nil, err
	}
	for i := range c.p.Fields {
		if f := &c.p.Fields[i]; f.Key == key && isEmpty(c.reply.Item.Fields[key]) {
			return f, of, nil
		}
	}
	return nil, nil, nil
}

// lastAsked returns the field of Seyal's last question, and the offer if it was one.
func (c *conversation) lastAsked() (field string, of *offer, err error) {
	if c.reply.Item.ID == "" {
		return "", nil, nil
	}
	var body string
	err = c.tx.QueryRow(c.ctx, `
		SELECT body FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id::text = $1
			AND kind = 'item.asked'
		ORDER BY seq DESC LIMIT 1`, c.reply.Item.ID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var e struct {
		Payload struct {
			Change struct {
				Field string `json:"field"`
				offer
			} `json:"change"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return "", nil, err
	}
	if e.Payload.Change.Name == "" {
		return e.Payload.Change.Field, nil, nil
	}
	return e.Payload.Change.Field, &e.Payload.Change.offer, nil
}

// answer fills field f from the person's answer, or says what was wrong with it. A name that isn't
// in a list is offered as a new entry instead (offered is then true, and nothing else is asked).
func (c *conversation) answer(f playbook.Field, text string) (offered bool, err error) {
	v, err := Understand(f, text, c.today, choices{c.ctx, c.tx})
	var notIn *NotInListError
	switch {
	case errors.As(err, &notIn) && f.Type == "master":
		return true, c.offerNew(f, tidyName(notIn.Name), nil)
	case err != nil:
		return false, c.say(fmt.Sprintf("Sorry, I didn't get that: %s.", strings.TrimSuffix(err.Error(), ".")))
	}
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.SetFields(c.ctx, sp, c.reply.Item.ID, engine.Fields{f.Key: v}, ledger.PersonActor(c.person))
	})
	if err != nil || problem == "" {
		return false, err
	}
	return false, c.say("Sorry, I couldn't use that: " + problem + ".")
}

// tidyName capitalises a name typed all in small letters ("sri balaji traders" → "Sri Balaji
// Traders"); a name with any capitals is kept as typed.
func tidyName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name != strings.ToLower(name) {
		return name
	}
	words := strings.Fields(name)
	for i, w := range words {
		r := []rune(w)
		words[i] = strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return strings.Join(words, " ")
}

// command runs a short command, if the text is one. Any other text is just a note.
func (c *conversation) command(text string) error {
	return c.commandBy(text, ledger.PersonActor(c.person))
}

// commandBy is command, recorded as done by the given actor (the person, or Seyal reading their message
// for them).
func (c *conversation) commandBy(text string, by ledger.Actor) error {
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
	case c.spoken:
		return c.say(voiceGuard)
	case waiting && (word == engine.Approve || word == engine.Reject):
		return c.decide(word, reason)
	}
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.Act(c.ctx, sp, it.ID, word, reason, by)
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
	roles, err := c.roles()
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
	msg := "Done. It is now " + strings.ReplaceAll(it.State, "_", " ") + "."
	if it.Team != "" {
		var name string
		if err := c.tx.QueryRow(c.ctx, `SELECT name FROM roles WHERE tenant_id = current_tenant() AND key = $1`,
			it.Team).Scan(&name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		msg += " It's with the " + cmp.Or(name, it.Team) + " team now."
	}
	return c.say(msg)
}

// askNext asks for the next empty required field. A question already asked is not repeated unless
// the person just tried to answer it. When everything is filled in after an answer, Seyal says how
// to move on.
func (c *conversation) askNext(answered bool) error {
	it := c.reply.Item
	if engine.IsClosed(c.p, it.State) {
		return nil
	}
	if f := NextField(c.p, it.State, it.Fields); f != nil {
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
	if !answered || len(engine.Missing(c.p, it.State, it.Fields)) > 0 {
		return nil
	}
	roles, err := c.roles()
	if err != nil {
		return err
	}
	// Name only a move this person may take (D68): a process can say per move who takes it.
	mayTake := engine.ActionsFor(c.p, it.Engine(), workitem.Actor(c.person, roles))
	ready := false
	for _, t := range c.p.Transitions {
		if t.When != "required_filled" || !slices.Contains(t.From, it.State) {
			continue
		}
		if slices.Contains(mayTake, t.Action) {
			return c.say(fmt.Sprintf("Everything needed is filled in. Say %q when it's ready.", strings.ReplaceAll(t.Action, "_", " ")))
		}
		ready = true
	}
	if ready {
		return c.say("Everything needed is filled in. Whoever has it can move it on now.")
	}
	return nil
}

// roles returns the roles the person in the conversation has now.
func (c *conversation) roles() ([]string, error) {
	rows, _ := c.tx.Query(c.ctx, `
		SELECT role_key FROM person_roles
		WHERE tenant_id = current_tenant() AND person_id::text = $1 AND revoked_at IS NULL`, c.person)
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
