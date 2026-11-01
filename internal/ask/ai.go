// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/master"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// AI reading of chat messages. The model only proposes; Seyal decides. Every value it proposes is
// checked with Understand, exactly as if the person had typed it as an answer, and saved as "Seyal
// on behalf of <person>". Approving, refusing and actions that need a reason are never taken from
// the model's reading: only when the person types the command itself. If AI is off, fails, or
// answers something unusable, the rule-based conversation carries on as before.
//
// What is sent to the model: the process's field names, labels, types, options and questions; the
// names of master items people can choose; which fields are already filled (not their values); and
// the recent messages, with authors shown only as "Person" or "Seyal".

// proposal is what the model answers.
type proposal struct {
	Kind     string            `json:"kind"` // new_work, answer, command, note, greeting, question
	Fields   map[string]string `json:"fields"`
	NewItems []newItem         `json:"new_items"`
	Command  string            `json:"command"`
	Reason   string            `json:"reason"`
	Reply    string            `json:"reply"`
}

type newItem struct {
	Field string `json:"field"`
	Name  string `json:"name"`
}

var kinds = []string{"new_work", "answer", "command", "note", "greeting", "question"}

var proposalSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"kind", "fields", "new_items", "command", "reason", "reply"},
	"properties": map[string]any{
		"kind":   map[string]any{"type": "string", "enum": kinds},
		"fields": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
		"new_items": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"field", "name"},
			"properties": map[string]any{"field": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}},
		}},
		"command": map[string]any{"type": "string"},
		"reason":  map[string]any{"type": "string"},
		"reply":   map[string]any{"type": "string"},
	},
}

// instructions is the stable part of the request: rules, the process's fields and the names people
// can choose. It changes only when the process or the master data changes, so providers can cache it.
func (c *conversation) instructions() (string, error) {
	var b strings.Builder
	b.WriteString(`You read messages that staff send to Seyal, a work assistant, about one kind of work item.
Answer only with JSON that follows the given schema. Never invent details that the message does not give.

kind:
- "new_work": the message describes new work to start.
- "answer": the message gives details for the current item (also answers to Seyal's question).
- "command": the message asks for one of the allowed actions; put its name in "command" and any reason in "reason".
- "note": a remark or report that needs no change.
- "greeting": hello, thanks, small talk. Put a short friendly reply in "reply".
- "question": a question to Seyal. Put a short, helpful reply in "reply"; never promise anything you can't do.

fields: for each detail the message gives, use the field's key and the value exactly as the person said it
("2k per month", "15/11", "Rasi"). Leave out details it doesn't give.
new_items: when a value names something that is not in a field's list, add {"field": key, "name": the name}.

Fields of this kind of work item:
`)
	for _, f := range c.p.Fields {
		if f.Readonly {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s, %s", f.Key, Label(f), typeWords(f))
		if f.Required {
			b.WriteString(", required")
		}
		if f.Ask != "" {
			fmt.Fprintf(&b, ` (Seyal asks: %q)`, f.Ask)
		}
		b.WriteString("\n")
		if f.Type == "master" {
			items, err := choices{c.ctx, c.tx}.Items(f.Kind)
			if err != nil {
				return "", err
			}
			names := make([]string, 0, len(items))
			for i, it := range items {
				if i == 200 {
					names = append(names, "…")
					break
				}
				names = append(names, it.Name)
			}
			fmt.Fprintf(&b, "  choices: %s\n", strings.Join(names, "; "))
		}
	}
	b.WriteString("\nAllowed actions: ")
	var actions []string
	for _, t := range c.p.Transitions {
		actions = append(actions, t.Action)
	}
	b.WriteString(strings.Join(actions, ", ") + "\n")
	return b.String(), nil
}

func typeWords(f playbook.Field) string {
	switch f.Type {
	case "choice":
		return "one of " + strings.Join(f.Options, " / ")
	case "list":
		return "list of " + f.Of
	case "master":
		return "one of the choices below"
	case "money":
		return "an amount of money"
	case "bool":
		return "yes or no"
	}
	return f.Type
}

// input is the changing part: the item's state, which fields are filled, Seyal's last question,
// the recent messages and the new one.
func (c *conversation) input(text, pendingKey string) (string, error) {
	var b strings.Builder
	it := c.reply.Item
	if it.ID != "" {
		fmt.Fprintf(&b, "State: %s\n", it.State)
		var filled []string
		for _, f := range c.p.Fields {
			if !isEmpty(it.Fields[f.Key]) {
				filled = append(filled, f.Key)
			}
		}
		fmt.Fprintf(&b, "Already filled: %s\n", strings.Join(filled, ", "))
		if pendingKey != "" {
			fmt.Fprintf(&b, "Seyal's last question was about: %s\n", pendingKey)
		}
		rows, _ := c.tx.Query(c.ctx, `
			SELECT author_kind, text FROM messages
			WHERE tenant_id = current_tenant() AND work_item_id::text = $1 AND text <> '' AND id::text <> $2
			ORDER BY sent_at DESC LIMIT 8`, it.ID, c.reply.Message.ID)
		recent, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Kind, Text string }])
		if err != nil {
			return "", err
		}
		if len(recent) > 0 {
			b.WriteString("Recent messages (oldest first):\n")
			for i := len(recent) - 1; i >= 0; i-- {
				who := "Person"
				if recent[i].Kind == "seyal" {
					who = "Seyal"
				}
				fmt.Fprintf(&b, "%s: %s\n", who, recent[i].Text)
			}
		}
	} else {
		b.WriteString("No work item yet: this message may start one.\n")
	}
	fmt.Fprintf(&b, "\nNew message from the person:\n%s\n", text)
	return b.String(), nil
}

// understand asks the model to read a message. It returns nil when AI is off, fails, or answers
// something unusable; the caller then carries on without AI.
func (c *conversation) understand(text, pendingKey string) *proposal {
	if _, on := c.env.AI.For(ai.Understand); !on || strings.TrimSpace(text) == "" {
		return nil
	}
	instr, err := c.instructions()
	if err != nil {
		return nil
	}
	in, err := c.input(text, pendingKey)
	if err != nil {
		return nil
	}
	resp, err := c.env.AI.Call(c.ctx, c.tx, ai.Understand,
		ai.Request{Instructions: instr, Input: in, Schema: proposalSchema, MaxTokens: 1024},
		ai.Link{PersonID: c.person, ItemID: c.reply.Item.ID})
	if err != nil {
		return nil
	}
	pr, err := parseProposal(resp.Text)
	if err != nil {
		return nil
	}
	return pr
}

func parseProposal(text string) (*proposal, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	var pr proposal
	if err := json.Unmarshal([]byte(text), &pr); err != nil {
		return nil, err
	}
	if !slices.Contains(kinds, pr.Kind) {
		return nil, fmt.Errorf("unknown kind %q", pr.Kind)
	}
	return &pr, nil
}

// apply carries out a proposal for an existing item. It reports whether the message counted as an
// answer, and whether Seyal asked about adding a new master item (then nothing else is asked).
func (c *conversation) apply(pr *proposal, text string, pending *playbook.Field) (answered, offered bool, err error) {
	switch pr.Kind {
	case "greeting", "question":
		if reply := strings.TrimSpace(pr.Reply); reply != "" {
			return false, false, c.say(limit(reply, 600))
		}
		return false, false, nil
	case "note":
		return false, false, nil
	case "command":
		return false, false, c.aiCommand(pr, text)
	}

	values := engine.Fields{}
	var problems []string
	var offer *newItem
	for _, key := range sortedKeys(pr.Fields) {
		f := c.field(key)
		if f == nil || f.Readonly {
			continue
		}
		v, err := Understand(*f, pr.Fields[key], c.today, choices{c.ctx, c.tx})
		if err == nil {
			values[key] = v
			continue
		}
		if f.Type == "master" && offer == nil {
			if i := slices.IndexFunc(pr.NewItems, func(n newItem) bool { return n.Field == key }); i >= 0 {
				offer = &pr.NewItems[i]
				continue
			}
		}
		problems = append(problems, fmt.Sprintf("%s: %s", Label(*f), strings.TrimSuffix(err.Error(), ".")))
	}

	// The model found nothing usable, but Seyal had asked something: read it as that answer.
	if len(values) == 0 && offer == nil && len(problems) == 0 && pending != nil {
		return true, false, c.answer(*pending, text)
	}
	if len(values) > 0 {
		problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
			return workitem.SetFields(c.ctx, sp, c.reply.Item.ID, values, ledger.AgentActor(c.person))
		})
		if err != nil {
			return true, false, err
		}
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		if err := c.say("I couldn't use all of that: " + strings.Join(problems, "; ") + "."); err != nil {
			return true, false, err
		}
	}
	if offer != nil {
		return true, true, c.offerNew(*c.field(offer.Field), offer.Name)
	}
	return true, false, nil
}

// aiCommand runs an action the model read from the message. Approving, refusing and actions that
// need a reason only happen when the person typed the command itself.
func (c *conversation) aiCommand(pr *proposal, text string) error {
	if c.isCommand(text) {
		return c.command(text)
	}
	for _, t := range c.p.Transitions {
		if t.Action == pr.Command && t.Needs == "" && slices.Contains(t.From, c.reply.Item.State) {
			return c.command(pr.Command)
		}
	}
	return c.say(`To do that, please type the command itself, for example "submit", or "cancel because …".`)
}

// isCommand reports whether the text is a command typed as such ("submit", "approve", "reject because
// ..."). Those are handled by the rules, without asking the model.
func (c *conversation) isCommand(text string) bool {
	it := c.reply.Item
	_, _, ok := Command(text, append(engine.Actions(c.p, engine.Item{State: it.State, Fields: it.Fields}),
		engine.Approve, engine.Reject))
	return ok
}

// offerNew asks whether to add a name that isn't in a field's list yet as a new master item.
func (c *conversation) offerNew(f playbook.Field, name string) error {
	label := strings.ToLower(Label(f))
	if mk := c.env.Library.MasterKind(f.Kind); mk != nil && mk.Label != "" {
		label = strings.ToLower(mk.Label)
	}
	m, err := c.post(fmt.Sprintf("%s isn't in the list yet. Add it as a new %s? (yes / no)", name, label))
	if err != nil {
		return err
	}
	_, err = ledger.Append(c.ctx, c.tx, ledger.Event{
		Kind: "item.asked", SubjectType: "work_item", SubjectID: c.reply.Item.ID,
		Payload: map[string]any{"change": map[string]any{"field": f.Key, "offer_new": name, "message_id": m.ID},
			"item": c.reply.Item},
	}.By(ledger.SystemActor))
	return err
}

// answerOffer handles the reply to "Add it as a new …?". Yes adds the master item (as the person who
// said yes) and fills the field; no asks the field's question again; anything else is read as an
// answer to the field.
func (c *conversation) answerOffer(f playbook.Field, name, text string) error {
	yes, err := parseYesNo(text)
	switch {
	case err != nil:
		return c.answer(f, text)
	case !yes:
		return nil // the field is still empty, so askNext asks its question again
	}
	mk := c.env.Library.MasterKind(f.Kind)
	if mk == nil {
		return c.say("I can't add new entries here. Please ask an admin to add it in Settings.")
	}
	by := ledger.PersonActor(c.person)
	problem, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		key, err := master.FreeKey(c.ctx, sp, f.Kind, name)
		if err != nil {
			return workitem.Item{}, err
		}
		if _, _, err := master.Save(c.ctx, sp, mk, key, name, engine.Fields{}, by); err != nil {
			return workitem.Item{}, err
		}
		return workitem.SetFields(c.ctx, sp, c.reply.Item.ID, engine.Fields{f.Key: key}, by)
	})
	switch {
	case err != nil:
		return err
	case problem != "":
		return c.say("Sorry, I couldn't add it: " + problem + ".")
	}
	return c.say(fmt.Sprintf("Added %s. An admin can fill in its details in Settings.", name))
}

func (c *conversation) field(key string) *playbook.Field {
	for i := range c.p.Fields {
		if c.p.Fields[i].Key == key {
			return &c.p.Fields[i]
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func limit(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// greetings are messages that start nothing, recognised without AI.
var greetings = []string{"hi", "hii", "hello", "hey", "hai", "good morning", "good afternoon", "good evening",
	"thanks", "thank you", "ok", "okay", "vanakkam", "வணக்கம்", "namaste", "namaskar"}

func isGreeting(text string) bool {
	t := strings.ToLower(strings.Trim(strings.TrimSpace(text), " .!,?"))
	return slices.Contains(greetings, t)
}
