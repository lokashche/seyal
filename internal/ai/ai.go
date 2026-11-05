// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ai is Seyal's single doorway to AI models. Each kind of AI work (a purpose) can use a
// different provider and model, set in the configuration; every call is recorded with its tokens,
// estimated cost and time, so a business can choose models from real numbers. When no model is set
// for a purpose, AI is simply off for it and callers carry on without it.
package ai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Purpose is what an AI call is for. Each purpose has its own model setting.
type Purpose string

const (
	Understand Purpose = "understand" // read a chat message: what it is, which details it gives
	Draft      Purpose = "draft"      // draft a process definition from a plain description
	Tidy       Purpose = "tidy"       // clean up a voice transcript without changing its meaning
	Summarize  Purpose = "summarize"  // summaries and reports
)

// Purposes lists every purpose, in the order the settings show them.
var Purposes = []Purpose{Understand, Draft, Tidy, Summarize}

// Request is one question to a model.
type Request struct {
	// Instructions is the stable part (rules, field lists, names), sent first so providers can
	// cache it; keep anything that changes per call out of it.
	Instructions string
	// Input is the part that changes: the person's message and recent context.
	Input string
	// Schema, when set, is the JSON schema the answer must follow.
	Schema map[string]any
	// MaxTokens limits the length of the answer.
	MaxTokens int
	// Timeout, when set, limits the whole call to the model (every try), for a caller that can't
	// wait long. The call is still recorded when it runs out.
	Timeout time.Duration
}

// Usage is the tokens one call used, the same way for every provider.
type Usage struct {
	Input      int // input tokens read at full price
	Cached     int // input tokens read from the provider's cache (cheaper)
	CacheWrite int // input tokens written to the cache (some providers charge extra)
	Output     int
}

// Response is a model's answer.
type Response struct {
	Text  string
	Usage Usage
}

// Model is one provider's model. Adding a provider means implementing this and registering it.
type Model interface {
	Complete(ctx context.Context, r Request) (Response, error)
}

// Price is what a model costs, in US dollars per million tokens. Prices change, so they come
// from the configuration, not from the code.
type Price struct {
	Input, Cached, CacheWrite, Output float64
}

// Cost estimates what a call cost, in US dollars.
func (p Price) Cost(u Usage) float64 {
	return (float64(u.Input)*p.Input + float64(u.Cached)*p.Cached +
		float64(u.CacheWrite)*p.CacheWrite + float64(u.Output)*p.Output) / 1e6
}

// Choice is the model set for one purpose.
type Choice struct {
	Name  string // "provider:model", as recorded
	Model Model
	Price Price
}

// Setup holds the model chosen for each purpose. The zero value has AI off everywhere.
type Setup struct {
	choices  map[Purpose]Choice
	speech   *Speech // voice notes to words (speech.go); nil = off
	USDtoINR float64 // for showing costs in rupees
}

// Set chooses the model for a purpose.
func (s *Setup) Set(p Purpose, c Choice) {
	if s.choices == nil {
		s.choices = map[Purpose]Choice{}
	}
	s.choices[p] = c
}

// For returns the model set for a purpose, if any.
func (s *Setup) For(p Purpose) (Choice, bool) {
	if s == nil {
		return Choice{}, false
	}
	c, ok := s.choices[p]
	return c, ok
}

// ErrOff means no model is set for this purpose; callers carry on without AI.
var ErrOff = errors.New("AI is not switched on for this")

// Link says who and what a call was for, for the cost report.
type Link struct {
	PersonID, ItemID string
}

// Call sends a request to the model set for its purpose and records the call (tokens, estimated
// cost, time, success). It returns ErrOff when AI is off for the purpose. tx must come from
// db.WithTenant; the record is written even when the model fails.
func (s *Setup) Call(ctx context.Context, tx pgx.Tx, p Purpose, r Request, link Link) (Response, error) {
	resp, call, callErr := s.Ask(ctx, p, r, link)
	if errors.Is(callErr, ErrOff) {
		return Response{}, ErrOff
	}
	if err := call.Record(ctx, tx); err != nil {
		return Response{}, err
	}
	return resp, callErr
}

// Ask is Call without the database: it sends the request and returns the answer and the call, which
// the caller records with Called.Record (also when the model failed). It is for callers that must
// not keep a database transaction open while the model works. It returns ErrOff (and no call to
// record) when AI is off for the purpose.
func (s *Setup) Ask(ctx context.Context, p Purpose, r Request, link Link) (Response, Called, error) {
	c, ok := s.For(p)
	if !ok {
		return Response{}, Called{}, ErrOff
	}
	start := time.Now()
	modelCtx := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		modelCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	resp, callErr := c.Model.Complete(modelCtx, r)
	call := Called{Purpose: p, Model: c.Name, Link: link, Usage: resp.Usage, CostUSD: c.Price.Cost(resp.Usage),
		Took: time.Since(start), Err: callErr}
	if callErr != nil {
		return Response{}, call, fmt.Errorf("ai: %s: %w", c.Name, callErr)
	}
	return resp, call, nil
}
