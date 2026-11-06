// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/workitem"
)

// Typed hand-overs in an item's thread (D68, section 6; build part 5; D73). handwords.go reads the
// words; this file finds who is meant and hands the item over with the same checks as the side panel
// (workitem.Pass, Take and Act), recorded as the person who typed it. A pass or take also says
// "via": "chat"; a send-back is recorded like the move's own word on the panel.
//
//   - Names are looked for only among the people the step lets receive the item (an admin may also
//     name any other member, as on the panel). A name that is someone else gets the reason they can't
//     have it; a name nobody has gets "I don't know Deb here" (decision 19).
//   - Two people with that name: "Which one do you mean?", with one button each for the person who
//     asked. A button sends the whole command again, with the full name. While the question is open,
//     that message (or the name on a button, typed out) means the person kept with the button, and
//     every check runs again (see chosen).
//   - A message that starts like a hand-over but isn't the whole command ("give to Ben the March file
//     when it comes") moves nothing. It stays a note, and Seyal says how to type it (decision 20; build
//     part 6 turns it into a Yes/No suggestion).

// notWholePass and notWholeBack are Seyal's hints for a message that starts like a hand-over but isn't
// the whole command. They name nobody, so they can go to the AI model like any reply.
const (
	notWholePass = `If you want to pass it on, type "pass to" and the name, then a comma and your note. I kept this as a note.`
	notWholeBack = `If you want to send it back, type "send back to" and the name or the step, then "because" and the reason. I kept this as a note.`
)

// handOver does the hand-over words typed in the thread, if text is one. handled is false when it
// isn't (or isn't for Seyal: the process has a move of that name), and the message is read as usual.
func (c *conversation) handOver(text string) (handled bool, err error) {
	moves := make([]string, 0, len(c.p.Transitions))
	for _, t := range c.p.Transitions {
		moves = append(moves, t.Action)
	}
	h, ok := readHandWords(text, moves)
	switch {
	case !ok:
		return false, nil
	case h.Do == doTake:
		return c.run(c.take)
	case h.Do == doSendBack:
		return c.sendBack(h)
	}
	return c.pass(h)
}

// run does what a typed hand-over leads to: the hand-over, a refusal, or "which one?". Words from a
// voice note get voiceGuard instead (decision 15). This is checked only once the words are known to be
// a real hand-over, so a spoken sentence that merely starts like one ("give it to them by Friday, …")
// is read as usual, and may still answer Seyal's question.
func (c *conversation) run(do func() error) (handled bool, err error) {
	if c.spoken {
		return true, c.say(voiceGuard)
	}
	return true, do()
}

// chosen does the option the person picked from Seyal's open "which one do you mean?": a button's own
// words, or its name typed out. The option keeps who or which move it means, so pressing "Ben" when
// there is also a "Ben Kumar" doesn't ask again. Every check runs again, as for a typed hand-over.
func (c *conversation) chosen(q Quick) (handled bool, err error) {
	if q.Person == "" && q.Move == "" { // asked before the buttons kept who or what they mean
		return c.handOver(q.Text)
	}
	if c.spoken {
		return true, c.say(voiceGuard)
	}
	k, err := c.kin()
	if err != nil {
		return false, err
	}
	if q.Person != "" {
		return true, c.passOn(workitem.PassOn{Person: q.Person, Note: q.Note, Via: "chat"}, k)
	}
	backs, err := workitem.BackMoves(c.ctx, c.tx, c.reply.Item, k.actor)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(backs, func(b workitem.BackMove) bool { return b.Action == q.Move })
	if i < 0 {
		return true, c.say("It can't go back there from here. " + k.canGoBack(backs))
	}
	return true, c.sendBackBy(backs[i], q.Note, k)
}

// take is "I'll take it".
func (c *conversation) take() error {
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.Take(c.ctx, sp, c.reply.Item.ID, "", "chat", ledger.PersonActor(c.person))
	})
	switch {
	case err != nil:
		return err
	case problem != "":
		return c.say(sentenceOf(problem))
	}
	return c.say("It's yours now.")
}

// pass is "pass to Dev, he knows them", "give back to the team", "pass to the Ops team".
func (c *conversation) pass(h handWords) (handled bool, err error) {
	it := c.reply.Item
	k, err := c.kin()
	if err != nil {
		return false, err
	}
	list := workitem.Receivers(c.p, it, k.members, k.roleNames)
	to := workitem.PassOn{Note: h.Note, Via: "chat"}
	if team, ok := teamWords(h.To); ok {
		role, why := c.teamFor(team, list, k.roleNames)
		if why != "" {
			return c.run(func() error { return c.say(why) })
		}
		to.Team = role
	} else {
		// Only the people this step allows (decision 19), and for an admin every other member too.
		var allowed []Choice
		for _, r := range list {
			if r.Person != "" {
				allowed = append(allowed, Choice{ID: r.Person, Name: r.Name})
			}
		}
		if k.actor.Admin {
			for _, m := range k.members {
				if m.ID != it.AssigneeID && !slices.ContainsFunc(allowed, func(a Choice) bool { return a.ID == m.ID }) {
					allowed = append(allowed, Choice{ID: m.ID, Name: m.Name})
				}
			}
		}
		found := whoIsNamed(h.To, allowed)
		anyone := whoIsNamed(h.To, k.people)
		switch {
		case len(found) > 1:
			options := make([]Quick, len(found))
			for i, p := range found {
				options[i] = Quick{Label: p.Name, Text: "pass to " + p.Name + withNote(", ", h.Note), Person: p.ID, Note: h.Note}
			}
			return c.run(func() error { return c.whichOne(found, options) })
		case len(found) == 1:
			to.Person = found[0].ID
		case len(anyone) == 1:
			to.Person = anyone[0].ID // someone who can't have it here: Pass says why
		case len(anyone) > 1:
			return c.run(func() error {
				return c.say(fmt.Sprintf("Nobody called %s can receive it in this step. %s", h.To, canGo(list)))
			})
		case startsWithName(h.To, k.people):
			return true, c.say(notWholePass) // spoken too: the hint moves nothing
		case mayBeName(h.To):
			return c.run(func() error { return c.say(fmt.Sprintf("I don't know %s here. %s", h.To, canGo(list))) })
		default:
			return false, nil // an ordinary sentence, read as usual
		}
	}
	return c.run(func() error { return c.passOn(to, k) })
}

// passOn passes the item on, as the person, and says how it went.
func (c *conversation) passOn(to workitem.PassOn, k kin) error {
	var notice string
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		changed, n, err := workitem.Pass(c.ctx, sp, c.reply.Item.ID, to, ledger.PersonActor(c.person))
		notice = n
		return changed, err
	})
	switch {
	case err != nil:
		return err
	case problem != "":
		return c.say(sentenceOf(problem))
	case strings.HasPrefix(notice, "Passed to"): // an admin passing outside the step's list says so itself
		return c.say(notice)
	}
	return c.say(strings.TrimSpace(fmt.Sprintf("Passed to %s. %s", k.holderName(c.reply.Item), notice)))
}

// teamFor finds the role a team's name means. name "" is "the team": the step's own team, or the only
// team the step lets receive the item. why says, when it finds none, what to type instead.
func (c *conversation) teamFor(name string, list []workitem.Receiver, roleNames map[string]string) (role, why string) {
	if name == "" {
		if kind, r := engine.Owner(c.p, c.reply.Item.State); kind == "role" {
			return r, ""
		}
		var teams []workitem.Receiver
		for _, r := range list {
			if r.Person == "" {
				teams = append(teams, r)
			}
		}
		switch len(teams) {
		case 1:
			return teams[0].Team, ""
		case 0:
			return "", "In this step it goes to one person, not to a team. " + canGo(list)
		}
		return "", "Which team do you mean? " + canGo(list)
	}
	for _, key := range slices.Sorted(maps.Keys(roleNames)) {
		if strings.EqualFold(name, roleNames[key]) || strings.EqualFold(name, strings.ReplaceAll(key, "_", " ")) {
			return key, ""
		}
	}
	return "", fmt.Sprintf("I don't know a team called %s here. %s", name, canGo(list))
}

// sendBack is "send back to Asha because …" or "send back to draft because …": a move marked back
// (D68, M4) found by the person who would get the item, or by the step.
func (c *conversation) sendBack(h handWords) (handled bool, err error) {
	it := c.reply.Item
	k, err := c.kin()
	if err != nil {
		return false, err
	}
	backs, err := workitem.BackMoves(c.ctx, c.tx, it, k.actor)
	if err != nil {
		return false, err
	}
	found := whoIsNamed(h.To, k.people)
	team, isTeam := teamWords(h.To)
	var hits []workitem.BackMove
	for _, b := range backs {
		switch {
		case isStep(h.To, b),
			b.Person != "" && slices.ContainsFunc(found, func(p Choice) bool { return p.ID == b.Person }),
			isTeam && b.Person == "" && b.Team != "" && (team == "" || strings.EqualFold(team, k.roleNames[b.Team]) ||
				strings.EqualFold(team, strings.ReplaceAll(b.Team, "_", " "))):
			hits = append(hits, b)
		}
	}
	if len(hits) > 0 && h.Note == "" {
		return c.run(func() error {
			return c.say(fmt.Sprintf(`Please add a reason, for example "send back to %s because …".`, h.To))
		})
	}
	switch {
	case len(hits) == 1:
		return c.run(func() error { return c.sendBackBy(hits[0], h.Note, k) })
	case len(hits) > 1:
		options := make([]Quick, len(hits))
		for i, b := range hits {
			// Two moves back to the same step are told apart by the move's own word.
			step, label := human(b.To), k.backLabel(b)
			if slices.ContainsFunc(hits, func(o workitem.BackMove) bool { return o.To == b.To && o.Action != b.Action }) {
				step, label = human(b.Action), human(b.Action)+": "+label
			}
			options[i] = Quick{Label: label, Text: "send back to " + step + " because " + h.Note, Move: b.Action, Note: h.Note}
		}
		return c.run(func() error { return c.whichOne(nil, options) })
	}
	// Nothing fits: say why, when the words mean someone or somewhere.
	where := k.canGoBack(backs)
	switch {
	case len(found) > 0:
		name := found[0].Name
		if slices.ContainsFunc(backs, func(b workitem.BackMove) bool {
			return slices.ContainsFunc(found, func(p Choice) bool { return p.ID == b.Last })
		}) {
			return c.run(func() error {
				return c.say(fmt.Sprintf("%s had it before, but can't have it back now (no longer active, no login, or not in the team any more). %s", name, where))
			})
		}
		return c.run(func() error {
			return c.say(fmt.Sprintf("%s hasn't had this in any step it can go back to. %s", name, where))
		})
	case isTeam || slices.ContainsFunc(c.p.States, func(s string) bool { return strings.EqualFold(h.To, human(s)) }):
		return c.run(func() error { return c.say("It can't go back there from here. " + where) })
	case startsWithName(h.To, k.people):
		return true, c.say(notWholeBack) // spoken too: the hint moves nothing
	case mayBeName(h.To):
		return c.run(func() error { return c.say(fmt.Sprintf("I don't know %s here. %s", h.To, where)) })
	}
	return false, nil
}

// sendBackBy sends the item back with move b, as the person, and says how it went. Like the move's own
// word on the panel, the state change is recorded as the person and the hand-over after it as Seyal
// (no "via": the typed message itself stays in the thread).
func (c *conversation) sendBackBy(b workitem.BackMove, reason string, k kin) error {
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.Act(c.ctx, sp, c.reply.Item.ID, b.Action, reason, ledger.PersonActor(c.person))
	})
	switch {
	case err != nil:
		return err
	case problem != "":
		return c.say(fmt.Sprintf("I couldn't send it back: %s.", strings.TrimSuffix(problem, ".")))
	}
	return c.say(fmt.Sprintf("Sent back to %s (%s).", k.holderName(c.reply.Item), human(c.reply.Item.State)))
}

// isStep reports whether words name the step a send-back goes to ("draft"), or the move itself ("back
// to sales", or just "sales").
func isStep(words string, b workitem.BackMove) bool {
	words = strings.Join(strings.Fields(words), " ")
	action := human(b.Action)
	return strings.EqualFold(words, human(b.To)) || strings.EqualFold(words, action) ||
		strings.EqualFold(words, strings.TrimPrefix(action, "back to "))
}

// whichOne asks "Which one do you mean?" with one button per option. The question is kept in the
// ledger (item.asked with "choose"), so the buttons are still there when the page is opened again; only
// the person who asked sees them (see Choosing). found are the people, when the options are people.
func (c *conversation) whichOne(found []Choice, options []Quick) error {
	labels := make([]string, len(options))
	seen := map[string]bool{}
	same := "" // a label that, read as a name, is the same as another ("Dev Example", "dev  example")
	for i, o := range options {
		labels[i] = o.Label
		if seen[plain(o.Label)] && same == "" {
			same = o.Label
		}
		seen[plain(o.Label)] = true
	}
	switch {
	case same != "" && len(found) > 0:
		return c.say(fmt.Sprintf("More than one person here is called %s, so I can't tell them apart by name. Use Pass on… in the side panel.", same))
	case same != "":
		return c.say("I can't tell these apart by name. Use the buttons in the side panel.")
	}
	m, err := c.post("Which one do you mean: " + orList(labels) + "?")
	if err != nil {
		return err
	}
	_, err = ledger.Append(c.ctx, c.tx, ledger.Event{
		Kind: "item.asked", SubjectType: "work_item", SubjectID: c.reply.Item.ID,
		Payload: map[string]any{"change": map[string]any{"choose": options, "for": c.person,
			"holder": c.reply.Item.HolderKey(), "message_id": m.ID}, "item": c.reply.Item},
	}.By(ledger.SystemActor))
	return err
}

// Quick is a reply button under the thread: Label on the button, Text sent as the person's message.
// A "which one?" button also keeps who or which move it means, and the note typed with it (see chosen).
type Quick struct {
	Label  string `json:"label"`
	Text   string `json:"text"`
	Person string `json:"person,omitempty"` // the person a "pass to" button passes to
	Move   string `json:"move,omitempty"`   // the move marked back a "send back to" button takes
	Note   string `json:"note,omitempty"`   // the note or reason typed with the hand-over
}

// Replies makes buttons that send their own words, such as "yes" and "no".
func Replies(words ...string) []Quick {
	out := make([]Quick, len(words))
	for i, w := range words {
		out[i] = Quick{Label: w, Text: w}
	}
	return out
}

// Choosing returns the buttons of Seyal's latest "which one do you mean?" in the item's thread, for
// person: only the person who asked sees them, and only until they write again in the thread or the
// item changes hands. It is nil otherwise. Each button sends the whole command again, so pressing an
// old one is no more than typing it. tx must come from db.WithTenant.
func Choosing(ctx context.Context, tx pgx.Tx, it workitem.Item, person string) ([]Quick, error) {
	ch, err := openChoice(ctx, tx, it, person, "")
	if err != nil || ch == nil {
		return nil, err
	}
	return ch.Choose, nil
}

// openChoice returns the item's latest "which one do you mean?" while it is open for person: asked for
// them, the item still with the same holder, and no message from them since, apart from the message
// with id except (the one being read now). It is nil otherwise.
func openChoice(ctx context.Context, tx pgx.Tx, it workitem.Item, person, except string) (*choice, error) {
	ch, err := latestChoice(ctx, tx, it.ID)
	if err != nil || ch == nil || ch.For != person || ch.Holder != it.HolderKey() {
		return nil, err
	}
	var since bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM messages m JOIN messages q ON q.tenant_id = m.tenant_id AND q.id::text = $3
			WHERE m.tenant_id = current_tenant() AND m.work_item_id::text = $1 AND m.author_kind = 'person'
				AND m.author_person_id::text = $2 AND m.sent_at > q.sent_at AND m.id::text <> $4)`,
		it.ID, person, ch.MessageID, except).Scan(&since); err != nil {
		return nil, fmt.Errorf("ask: choosing: %w", err)
	}
	if since {
		return nil, nil
	}
	return ch, nil
}

// picked returns the option a reply to "which one?" means: a button's own words, the words on one
// button typed out ("Ben Sample", "ben sample."), or a first word only one button has ("draft"). ok is
// false when the reply doesn't pick exactly one.
func picked(options []Quick, text string) (q Quick, ok bool) {
	text = strings.TrimSpace(text)
	for _, o := range options {
		if o.Text == text {
			return o, true
		}
	}
	var hits []Quick
	for _, o := range options {
		if plain(o.Label) == plain(text) {
			hits = append(hits, o)
		}
	}
	if len(hits) == 0 {
		labels := make([]Choice, len(options))
		for i, o := range options {
			labels[i] = Choice{ID: strconv.Itoa(i), Name: o.Label}
		}
		for _, l := range whoIsNamed(plain(text), labels) {
			i, _ := strconv.Atoi(l.ID)
			hits = append(hits, options[i])
		}
	}
	if len(hits) != 1 {
		return Quick{}, false
	}
	return hits[0], true
}

// choiceTexts returns the words of every "which one?" button asked on the item.
func choiceTexts(ctx context.Context, tx pgx.Tx, itemID string) ([]string, error) {
	changes, err := askedChanges(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, raw := range changes {
		var ch choice
		if err := json.Unmarshal(raw, &ch); err != nil {
			return nil, fmt.Errorf("ask: questions: %w", err)
		}
		for _, q := range ch.Choose {
			out = append(out, q.Text)
		}
	}
	return out, nil
}

// plain is words as a name is compared: small letters, single spaces, no full stop or "!" at the end.
func plain(s string) string {
	return strings.TrimRight(strings.ToLower(strings.Join(strings.Fields(s), " ")), " .!")
}

// choice is a "which one do you mean?" as kept in an item.asked event.
type choice struct {
	Choose    []Quick `json:"choose"`
	For       string  `json:"for"`
	Holder    string  `json:"holder"`
	MessageID string  `json:"message_id"`
}

// askedChanges returns the "change" of the item's item.asked events, newest first.
func askedChanges(ctx context.Context, tx pgx.Tx, itemID string) ([]json.RawMessage, error) {
	rows, _ := tx.Query(ctx, `
		SELECT body FROM ledger
		WHERE tenant_id = current_tenant() AND subject_type = 'work_item' AND subject_id = $1::uuid
			AND kind = 'item.asked'
		ORDER BY seq DESC`, itemID)
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("ask: questions: %w", err)
	}
	out := make([]json.RawMessage, 0, len(bodies))
	for _, body := range bodies {
		var e struct {
			Payload struct {
				Change json.RawMessage `json:"change"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("ask: questions: %w", err)
		}
		out = append(out, e.Payload.Change)
	}
	return out, nil
}

// latestChoice returns the item's latest "which one do you mean?", or nil.
func latestChoice(ctx context.Context, tx pgx.Tx, itemID string) (*choice, error) {
	changes, err := askedChanges(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	for _, raw := range changes {
		var ch choice
		if err := json.Unmarshal(raw, &ch); err != nil {
			return nil, fmt.Errorf("ask: questions: %w", err)
		}
		if len(ch.Choose) > 0 {
			return &ch, nil
		}
	}
	return nil, nil
}

// kin is who is around when a hand-over is typed: everyone in the business (to recognise a name),
// the members work can go to, the role names, and the person typing.
type kin struct {
	people    []Choice // every person of the business, also those who left or have no login
	members   []workitem.Member
	roleNames map[string]string
	actor     engine.Actor
}

func (c *conversation) kin() (kin, error) {
	var k kin
	rows, _ := c.tx.Query(c.ctx, `SELECT id::text, display_name FROM persons WHERE tenant_id = current_tenant() ORDER BY display_name, id`)
	people, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Choice])
	if err != nil {
		return kin{}, fmt.Errorf("ask: people: %w", err)
	}
	k.people = people
	if k.members, err = workitem.Members(c.ctx, c.tx); err != nil {
		return kin{}, err
	}
	rows, _ = c.tx.Query(c.ctx, `SELECT key, name FROM roles WHERE tenant_id = current_tenant()`)
	roles, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key, Name string }])
	if err != nil {
		return kin{}, fmt.Errorf("ask: roles: %w", err)
	}
	k.roleNames = map[string]string{}
	for _, r := range roles {
		k.roleNames[r.Key] = r.Name
	}
	mine, err := c.roles()
	if err != nil {
		return kin{}, err
	}
	k.actor = workitem.Actor(c.person, mine)
	return k, nil
}

// name is a person's name, or "someone".
func (k kin) name(id string) string {
	if i := slices.IndexFunc(k.people, func(p Choice) bool { return p.ID == id }); i >= 0 {
		return k.people[i].Name
	}
	return "someone"
}

// holderName names who has the item: a person, a team, or nobody.
func (k kin) holderName(it workitem.Item) string {
	switch {
	case it.AssigneeID != "":
		return k.name(it.AssigneeID)
	case it.Team != "":
		return workitem.TeamName(it.Team, k.roleNames)
	}
	return "nobody"
}

// backLabel names a send-back for a button: "draft (Asha Example)".
func (k kin) backLabel(b workitem.BackMove) string {
	switch {
	case b.Person != "":
		return human(b.To) + " (" + k.name(b.Person) + ")"
	case b.Team != "":
		return human(b.To) + " (" + workitem.TeamName(b.Team, k.roleNames) + ")"
	}
	return human(b.To)
}

// canGoBack says where the item can be sent back to from here.
func (k kin) canGoBack(backs []workitem.BackMove) string {
	if len(backs) == 0 {
		return "Nothing sends work back from this step."
	}
	labels := make([]string, len(backs))
	for i, b := range backs {
		labels[i] = k.backLabel(b)
	}
	return "It can go back to: " + orList(labels) + "."
}

// canGo says who may receive the item in this step: "It can go to: Dev, or the Ops team."
func canGo(list []workitem.Receiver) string {
	if len(list) == 0 {
		return "Nobody else can receive it in this step; an admin can pass it on."
	}
	names := make([]string, len(list))
	for i, r := range list {
		names[i] = r.Name
	}
	return "It can go to: " + orList(names) + "."
}

// orList joins names as people read them: "Ben, Dev, or the Ops team".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}

func withNote(sep, note string) string {
	if note == "" {
		return ""
	}
	return sep + note
}

// human makes a key readable: "with_ops" → "with ops".
func human(key string) string { return strings.ReplaceAll(key, "_", " ") }

// sentenceOf turns a refusal into a sentence: a capital letter first, a full stop at the end.
func sentenceOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	s = strings.ToUpper(string(r[0])) + string(r[1:])
	if !strings.ContainsAny(s[len(s)-1:], ".!?") {
		s += "."
	}
	return s
}
