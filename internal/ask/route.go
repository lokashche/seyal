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
}

// Routing is what Seyal made of a message on Tell Seyal.
type Routing struct {
	Message   channel.Message
	Suggested string            // the version id of the most likely process, or "" if unsure
	Seyal     []channel.Message // Seyal's reply, when the message starts nothing (e.g. a greeting)
}

// Route saves a message from Tell Seyal and suggests the process it is for. A greeting or a
// question gets a reply instead, and nothing is suggested. tx must come from db.WithTenant.
func Route(ctx context.Context, tx pgx.Tx, env Env, processes []Process, in channel.Incoming) (Routing, error) {
	in.ThreadID = ""
	msg, _, err := channel.Receive(ctx, tx, env.Store, in)
	if err != nil {
		return Routing{}, err
	}
	r := Routing{Message: msg}
	text := strings.TrimSpace(msg.Text)
	kind, suggested, reply := routeWithAI(ctx, tx, env, processes, msg.PersonID, text)
	if kind == "" { // AI off or no usable answer
		if isGreeting(text) {
			kind, reply = "greeting", greetingReply
		} else {
			kind, suggested = "work", routeByWords(processes, text)
		}
	}
	if (kind == "greeting" || kind == "question") && len(msg.Media) == 0 {
		m, err := channel.Post(ctx, tx, "", limit(strings.TrimSpace(reply), 600))
		if err != nil {
			return Routing{}, err
		}
		r.Seyal = append(r.Seyal, m)
		return r, nil
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
- kind "question": a question to Seyal. Put a short, helpful reply in "reply"; never promise anything.

Processes:
`)
	for _, p := range processes {
		fmt.Fprintf(&b, "- id %s: %s. %s", p.ID, p.Name, p.Description)
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
// the message (words of three letters or more). It returns "" when nothing matches.
func routeByWords(processes []Process, text string) string {
	words := wordSet(text)
	best, bestScore := "", 0
	for _, p := range processes {
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

func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		if len([]rune(w)) >= 3 {
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
	if !offered {
		if err := c.askNext(false); err != nil {
			return Reply{}, err
		}
	}
	return c.reply, nil
}
