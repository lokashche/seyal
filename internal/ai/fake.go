// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"context"
	"errors"
	"sync"
)

// Fake is a stand-in model for tests and demos. It understands nothing: it returns answers
// prepared in advance, so tests can check what Seyal does with an answer (validate it, fall back,
// record the cost) without a real provider, a key, or any cost.
type Fake struct {
	// Answer returns the prepared answer for a request. If nil, Fake answers "{}".
	Answer func(r Request) (string, error)
	// Usage is reported for every call, so cost recording can be tested.
	Usage Usage

	mu   sync.Mutex
	seen []Request
}

// Complete returns the prepared answer.
func (f *Fake) Complete(ctx context.Context, r Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	f.mu.Lock()
	f.seen = append(f.seen, r)
	f.mu.Unlock()
	if f.Answer == nil {
		return Response{Text: "{}", Usage: f.Usage}, nil
	}
	text, err := f.Answer(r)
	if err != nil {
		return Response{Usage: f.Usage}, err
	}
	return Response{Text: text, Usage: f.Usage}, nil
}

// Seen returns the requests the fake received, so tests can check what Seyal sent.
func (f *Fake) Seen() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.seen...)
}

// ErrFakeDown is a ready-made failure for tests of what happens when a provider is down.
var ErrFakeDown = errors.New("fake provider is down")
