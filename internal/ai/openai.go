// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Services that speak the OpenAI-style chat API ("chat/completions"). Many providers and local servers
// do, so one small connector covers them. The same presets also offer speech to text
// ("audio/transcriptions", whisper.go). Each preset has its own settings in the private .env:
//
//	groq:<model>    SEYAL_GROQ_API_KEY (and optionally SEYAL_GROQ_BASE_URL)
//	openai:<model>  SEYAL_OPENAI_API_KEY (and optionally SEYAL_OPENAI_BASE_URL)
//	compat:<model>  SEYAL_COMPAT_BASE_URL, e.g. http://localhost:11434/v1 for Ollama, and
//	                SEYAL_COMPAT_API_KEY if the service needs one
type preset struct {
	name, baseURL string
	needsKey      bool
	// newerTokens: the service takes max_completion_tokens; older or local ones take max_tokens.
	newerTokens bool
}

var presets = []preset{
	{name: "groq", baseURL: "https://api.groq.com/openai/v1", needsKey: true, newerTokens: true},
	{name: "openai", baseURL: "https://api.openai.com/v1", needsKey: true, newerTokens: true},
	{name: "compat", needsKey: false},
}

func init() {
	for _, p := range presets {
		Register(p.name, func(model string, get func(string) string) (Model, error) { return newChat(p, model, get) })
		RegisterSpeech(p.name, func(model string, get func(string) string) (Transcriber, error) {
			return newWhisper(p, model, get)
		})
	}
}

// connect reads a preset's address and key from the settings: SEYAL_<PRESET>_BASE_URL (optional for
// the named services) and SEYAL_<PRESET>_API_KEY. Chat models and speech share them.
func (p preset) connect(get func(string) string) (base, key string, err error) {
	env := "SEYAL_" + strings.ToUpper(p.name)
	base = strings.TrimSpace(get(env + "_BASE_URL"))
	if base == "" {
		base = p.baseURL
	}
	if base == "" {
		return "", "", fmt.Errorf("set %s_BASE_URL to the service's address", env)
	}
	key = strings.TrimSpace(get(env + "_API_KEY"))
	if p.needsKey && key == "" {
		return "", "", fmt.Errorf("set %s_API_KEY in the private .env file", env)
	}
	return strings.TrimSuffix(base, "/"), key, nil
}

func newChat(p preset, model string, get func(string) string) (Model, error) {
	base, key, err := p.connect(get)
	if err != nil {
		return nil, err
	}
	return &chat{
		provider: p.name, model: model, url: base + "/chat/completions", key: key,
		newerTokens: p.newerTokens,
	}, nil
}

// chat is one model at an OpenAI-style service.
type chat struct {
	provider, model, url, key string
	newerTokens               bool

	mu         sync.Mutex
	noSchemaOK bool // the model refused a JSON schema once; ask for plain JSON from then on
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReply struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// timeout limits one request; drafting a long definition can take a while.
const timeout = 2 * time.Minute

var (
	// errSchemaRefused means the model doesn't support answering to a JSON schema.
	errSchemaRefused = errors.New("schema refused")
	// errSchemaMissed means the service rejected one answer for not fitting the schema (e.g. the model
	// spent its tokens thinking and returned nothing).
	errSchemaMissed = errors.New("answer didn't fit the schema")
)

// Complete sends one request. When a schema is given, the model is asked to follow it; a model that
// can't is asked for plain JSON instead, with the schema written into the instructions. Seyal checks
// every answer anyway.
func (c *chat) Complete(ctx context.Context, r Request) (Response, error) {
	c.mu.Lock()
	plain := c.noSchemaOK
	c.mu.Unlock()
	if r.Schema != nil && !plain {
		resp, err := c.send(ctx, r, true)
		switch {
		case errors.Is(err, errSchemaRefused):
			c.mu.Lock()
			c.noSchemaOK = true
			c.mu.Unlock()
		case errors.Is(err, errSchemaMissed):
			// This answer didn't fit the schema; try once in plain JSON, but keep using the schema.
		default:
			return resp, err
		}
	}
	return c.send(ctx, r, false)
}

func (c *chat) send(ctx context.Context, r Request, withSchema bool) (Response, error) {
	instructions := r.Instructions
	body := map[string]any{"model": c.model}
	switch {
	case r.Schema != nil && withSchema:
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "answer", "schema": r.Schema}}
	case r.Schema != nil:
		schema, err := json.Marshal(r.Schema)
		if err != nil {
			return Response{}, err
		}
		instructions += "\n\nAnswer with one JSON object that follows this JSON schema:\n" + string(schema) + "\n"
		body["response_format"] = map[string]any{"type": "json_object"}
	}
	body["messages"] = []chatMessage{{Role: "system", Content: instructions}, {Role: "user", Content: r.Input}}
	if r.MaxTokens > 0 {
		if c.newerTokens {
			body["max_completion_tokens"] = r.MaxTokens
		} else {
			body["max_tokens"] = r.MaxTokens
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%s: %w", c.provider, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%s: reading the answer: %w", c.provider, err)
	}
	if res.StatusCode != http.StatusOK {
		if body := string(raw); withSchema && res.StatusCode == http.StatusBadRequest {
			switch {
			case strings.Contains(body, "json_validate_failed"):
				return Response{}, errSchemaMissed
			case strings.Contains(body, "response_format") || strings.Contains(body, "json_schema"):
				return Response{}, errSchemaRefused
			}
		}
		return Response{}, fmt.Errorf("%s answered %s: %s", c.provider, res.Status, serviceSaid(raw))
	}
	var reply chatReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return Response{}, fmt.Errorf("%s: the answer can't be read: %w", c.provider, err)
	}
	u := reply.Usage
	usage := Usage{Input: u.PromptTokens - u.PromptTokensDetails.CachedTokens, Cached: u.PromptTokensDetails.CachedTokens,
		Output: u.CompletionTokens}
	if len(reply.Choices) == 0 {
		return Response{Usage: usage}, fmt.Errorf("%s: the answer was empty", c.provider)
	}
	return Response{Text: reply.Choices[0].Message.Content, Usage: usage}, nil
}

// serviceSaid is what a service's error answer says, for the error recorded in AI usage: the message
// of an OpenAI-style error ({"error": {"message": ...}}), else the answer itself, cut to 300 bytes.
// Only the message, because some services add what the model wrote to the error (Groq's
// "failed_generation"), which can hold the words of a voice note; AI usage rows are kept for good,
// also when the person discards the note (D66).
func serviceSaid(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && strings.TrimSpace(e.Error.Message) != "" {
		msg = strings.TrimSpace(e.Error.Message)
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}
