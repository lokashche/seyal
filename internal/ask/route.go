// SPDX-License-Identifier: AGPL-3.0-or-later

package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/channel"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/workitem"
)

// Working out which process a message on Tell Seyal is for. The message is saved first (with its
// voice note or files), then Seyal suggests a process and the person confirms it with one tap
// (D47). Nothing starts until they do. With AI, the model reads the processes' descriptions and
// examples; without it, the words of the message are matched against them.

// Process is one process people can start, as routing sees it.
type Process struct {
	VersionID, ID, Name, Description string
	Examples                         []string
	Fallback                         bool // suggested when no other process fits
}

// Routing is what Seyal made of a message on Tell Seyal.
type Routing struct {
	Message   channel.Message
	Suggested string            // the version id of the most likely process, or "" if unsure
	Seyal     []channel.Message // Seyal's reply, when the message starts nothing (e.g. a greeting)
	Answer    *Answer           // Seyal's answer, when it can't read a question now (D67)
	Asked     *Asked            // a question about the work, to answer with AnswerAsked in a new transaction
}

// Route saves a message from Tell Seyal and suggests the process it is for. A greeting gets a reply
// instead, and a question about the work comes back as Asked; nothing is suggested for either.
// tx must come from db.WithTenant.
func Route(ctx context.Context, tx pgx.Tx, env Env, processes []Process, in channel.Incoming) (Routing, error) {
	in.ThreadID = ""
	msg, isNew, err := channel.Receive(ctx, tx, env.Store, in)
	if err != nil {
		return Routing{}, err
	}
	r := Routing{Message: msg}
	if !isNew && msg.ThreadID != "" {
		return r, nil // sent again after it started an item: the caller opens that item (D93)
	}
	text := strings.TrimSpace(msg.Text)
	var kind, suggested, reply string
	// A message sent again is never read again (no second AI call, no second reply), and shared words
	// never go to the model: for both, only the process is suggested, from the words (D93).
	again := !isNew || msg.Via == channel.ViaShare
	if !again {
		kind, suggested, reply = routeWithAI(ctx, tx, env, processes, msg.PersonID, text)
	}
	if kind == "" { // AI off, no usable answer, or not read again
		switch {
		case again:
			kind, suggested = "work", routeByWords(processes, text)
		case isGreeting(text):
			kind, reply = "greeting", greetingReply
		case looksLikeQuestion(text) && wordsOnly(msg): // a clear question: Seyal can't answer it now
			r.Answer = Unread(env, msg.ID, text, "")
			return r, nil
		default:
			kind, suggested = "work", routeByWords(processes, text)
		}
	}
	if kind == "question" && wordsOnly(msg) { // answered from the data after this transaction (D67)
		r.Asked = &Asked{MessageID: msg.ID, PersonID: msg.PersonID, Question: text, Reply: reply}
		return r, nil
	}
	if kind == "greeting" && wordsOnly(msg) {
		m, err := channel.Post(ctx, tx, "", limit(strings.TrimSpace(reply), 600))
		if err != nil {
			return Routing{}, err
		}
		r.Seyal = append(r.Seyal, m)
		return r, nil
	}
	if suggested == "" { // nothing fits: offer the catch-all, if the business has one
		for _, p := range processes {
			if p.Fallback {
				suggested = p.VersionID
				break
			}
		}
	}
	r.Suggested = suggested
	return r, nil
}

// routeWithAI asks the model which process the message is for. kind is "" when AI is off or its
// answer can't be used; suggested is a version id, or "".
func routeWithAI(ctx context.Context, tx pgx.Tx, env Env, processes []Process, personID, text string) (kind, suggested, reply string) {
	if _, on := env.AI.For(ai.Understand); !on || text == "" {
		return "", "", ""
	}
	var b strings.Builder
	b.WriteString(`You help Seyal, a work assistant, decide which process a message from staff is for.
Answer only with JSON that follows the given schema.
- kind "work": the message is about work to start or report. Put the id of the best matching process in
  "process", or "" if none fits or you can't tell.
- kind "greeting": hello, thanks, small talk. Put a short friendly reply in "reply".
- kind "question": a question to Seyal, including questions about the work (what is open, overdue, done, by
  whom; Seyal looks those up itself). Put a short, helpful reply in "reply"; never promise anything.

Processes:
`)
	for _, p := range processes {
		fmt.Fprintf(&b, "- id %s: %s. %s", p.ID, p.Name, p.Description)
		if p.Fallback {
			b.WriteString(" Choose this only when no other process fits.")
		}
		if len(p.Examples) > 0 {
			fmt.Fprintf(&b, " Examples: %q", p.Examples)
		}
		b.WriteString("\n")
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"kind", "process", "reply"},
		"properties": map[string]any{
			"kind":    map[string]any{"type": "string", "enum": []string{"work", "greeting", "question"}},
			"process": map[string]any{"type": "string"},
			"reply":   map[string]any{"type": "string"},
		},
	}
	resp, err := env.AI.Call(ctx, tx, ai.Understand, ai.Request{Instructions: b.String(),
		Input: "Message:\n" + text + "\n", Schema: schema, MaxTokens: 2048}, ai.Link{PersonID: personID})
	if err != nil {
		return "", "", ""
	}
	var a struct{ Kind, Process, Reply string }
	raw := strings.TrimSpace(resp.Text)
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(raw, "```json"), "```"), "```"))
	if err := json.Unmarshal([]byte(raw), &a); err != nil || !slices.Contains([]string{"work", "greeting", "question"}, a.Kind) {
		return "", "", ""
	}
	for _, p := range processes {
		if p.ID == a.Process {
			suggested = p.VersionID
		}
	}
	if a.Kind != "work" && strings.TrimSpace(a.Reply) == "" {
		a.Reply = greetingReply
	}
	return a.Kind, suggested, a.Reply
}

// routeByWords suggests the process whose name, description and examples share the most words with
// the message (words of three letters or more). It returns "" when nothing matches. The catch-all
// takes no part: its words ("anything", "task") would match too much.
func routeByWords(processes []Process, text string) string {
	words := wordSet(text)
	best, bestScore := "", 0
	for _, p := range processes {
		if p.Fallback {
			continue
		}
		known := wordSet(strings.Join(append([]string{p.Name, p.Description}, p.Examples...), " "))
		score := 0
		for w := range words {
			if known[w] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = p.VersionID, score
		}
	}
	return best
}

// commonWords say nothing about which process a message is for, so they never count as a match.
var commonWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true, "that": true, "from": true, "into": true,
	"are": true, "was": true, "were": true, "has": true, "have": true, "had": true, "our": true, "your": true,
	"you": true, "its": true, "can": true, "will": true, "not": true, "but": true, "all": true, "any": true,
	"about": true, "please": true, "then": true, "them": true, "they": true, "who": true, "what": true,
	"when": true, "how": true, "out": true, "off": true, "too": true, "also": true, "again": true,
}

func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		if len([]rune(w)) >= 3 && !commonWords[w] {
			set[w] = true
		}
	}
	return set
}

// BeginFrom starts a work item from a message already saved on Tell Seyal (by Route), once the
// person has confirmed the process. Only the person who wrote it may, and only while it starts
// nothing else. tx must come from db.WithTenant.
func BeginFrom(ctx context.Context, tx pgx.Tx, env Env, playbookVersionID, messageID, personID string) (Reply, error) {
	msg, err := channel.Get(ctx, tx, messageID)
	if err != nil || msg.PersonID != personID || msg.ThreadID != "" || msg.AuthorKind != "person" {
		return Reply{}, fmt.Errorf("that message can't be started any more; please write it again")
	}
	c, err := newConversation(ctx, tx, env, playbookVersionID, workitem.Item{}, msg)
	if err != nil {
		return Reply{}, err
	}
	return c.start(c.understand(strings.TrimSpace(msg.Text), ""))
}

// start creates the work item for the conversation's message, files the message into it, fills
// what the model read (if anything), and asks for what is missing.
func (c *conversation) start(pr *proposal) (Reply, error) {
	msg := c.reply.Message
	by := ledger.PersonActor(msg.PersonID)
	it, err := workitem.Start(c.ctx, c.tx, workitem.NewItem{PlaybookVersionID: c.versionID, Via: "chat", Ref: msg.ID, By: by})
	if err != nil {
		return Reply{}, err
	}
	if c.reply.Message, err = channel.FileInto(c.ctx, c.tx, msg.ID, it.ID, by); err != nil {
		return Reply{}, err
	}
	c.reply.Item = it
	offered := false
	if pr != nil {
		if _, offered, err = c.apply(pr, strings.TrimSpace(msg.Text), nil); err != nil {
			return Reply{}, err
		}
	}
	if err := c.fromMessage(strings.TrimSpace(msg.Text)); err != nil {
		return Reply{}, err
	}
	if err := c.flush(); err != nil {
		return Reply{}, err
	}
	if !offered && !c.suggested {
		if err := c.askNext(false); err != nil {
			return Reply{}, err
		}
	}
	return c.reply, nil
}

// fromMessage fills the fields marked from_message that are still empty with the starting message,
// so Seyal doesn't ask for what the person has just said.
func (c *conversation) fromMessage(text string) error {
	if text == "" {
		return nil
	}
	values := engine.Fields{}
	for _, f := range c.p.Fields {
		if f.FromMessage && c.reply.Item.Fields[f.Key] == nil {
			values[f.Key] = text
		}
	}
	if len(values) == 0 {
		return nil
	}
	_, err := c.try(func(sp pgx.Tx) (workitem.Item, error) {
		return workitem.SetFields(c.ctx, sp, c.reply.Item.ID, values, ledger.PersonActor(c.reply.Message.PersonID))
	})
	return err
}

// Nudge asks the next question, if any, in an item and in the items linked to it (the one it is part
// of, and those it started), for when something other than a message in their own thread changed them:
// a button, or linked work starting or reporting back. A question already asked isn't repeated.
// tx must come from db.WithTenant.
func Nudge(ctx context.Context, tx pgx.Tx, env Env, itemID string) error {
	if err := askIn(ctx, tx, env, []string{itemID}); err != nil {
		return err
	}
	return nudgeLinked(ctx, tx, env, itemID)
}
