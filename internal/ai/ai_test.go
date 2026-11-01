// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestCost(t *testing.T) {
	// The example from the planning notes: 1,000 fresh + 3,000 cached input, 600 output tokens,
	// at $4 / $0.20 / $5 / $20 per million.
	p := Price{Input: 4, Cached: 0.20, CacheWrite: 5, Output: 20}
	got := p.Cost(Usage{Input: 1000, Cached: 3000, Output: 600})
	if want := 0.004 + 0.0006 + 0.012; math.Abs(got-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", got, want)
	}
	if (Price{}).Cost(Usage{Input: 1e6}) != 0 {
		t.Error("an unknown price should cost 0, not guess")
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	s, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, on := s.For(Understand); on || s.USDtoINR != 88 {
		t.Errorf("empty settings: AI on=%v, rate %v; want off and 88", on, s.USDtoINR)
	}

	s, err = FromEnv(env(map[string]string{
		"SEYAL_AI_UNDERSTAND": "fake:demo", "SEYAL_AI_UNDERSTAND_PRICE": "2, 0.2, 2.5, 10", "SEYAL_USD_INR": "90",
	}))
	if err != nil {
		t.Fatal(err)
	}
	c, on := s.For(Understand)
	if !on || c.Name != "fake:demo" || c.Price.Output != 10 || s.USDtoINR != 90 {
		t.Errorf("understand = %+v (on=%v), rate %v", c, on, s.USDtoINR)
	}
	if _, on := s.For(Draft); on {
		t.Error("draft is on although nothing was set for it")
	}

	for _, bad := range []map[string]string{
		{"SEYAL_AI_DRAFT": "no-colon"},
		{"SEYAL_AI_DRAFT": "nosuchprovider:x"},
		{"SEYAL_AI_DRAFT": "fake:x", "SEYAL_AI_DRAFT_PRICE": "1,2,3"},
		{"SEYAL_USD_INR": "lots"},
	} {
		if _, err := FromEnv(env(bad)); err == nil {
			t.Errorf("settings %v were accepted", bad)
		}
	}
	_, err = FromEnv(env(map[string]string{"SEYAL_AI_DRAFT": "nosuchprovider:x"}))
	if err == nil || !strings.Contains(err.Error(), "available: fake") {
		t.Errorf("unknown provider error = %v, want the list of available providers", err)
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Answer: func(r Request) (string, error) { return `{"echo":"` + r.Input + `"}`, nil }, Usage: Usage{Input: 5}}
	resp, err := f.Complete(context.Background(), Request{Input: "hi"})
	if err != nil || resp.Text != `{"echo":"hi"}` || resp.Usage.Input != 5 || len(f.Seen()) != 1 {
		t.Errorf("fake = %+v, %v, seen %d", resp, err, len(f.Seen()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Complete(ctx, Request{}); err == nil {
		t.Error("fake ignored a cancelled request")
	}
}

func TestOffByDefault(t *testing.T) {
	var s *Setup
	if _, on := s.For(Understand); on {
		t.Error("a nil setup has AI on")
	}
}
