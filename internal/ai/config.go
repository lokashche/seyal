// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Providers turn a model name into a Model. Each provider registers itself here: "fake" (tests and
// demos) and the OpenAI-style services in openai.go. AI stays off until a purpose is configured.
var (
	providersMu sync.Mutex
	providers   = map[string]func(model string, get func(string) string) (Model, error){
		"fake": func(string, func(string) string) (Model, error) { return &Fake{}, nil },
	}
)

// Register makes a provider available to the configuration, e.g. Register("openai", newOpenAI).
// get reads settings such as the provider's API key.
func Register(name string, open func(model string, get func(string) string) (Model, error)) {
	providersMu.Lock()
	defer providersMu.Unlock()
	providers[name] = open
}

// FromEnv reads the AI settings. For each purpose, SEYAL_AI_<PURPOSE> is "provider:model" (empty
// = off) and SEYAL_AI_<PURPOSE>_PRICE is "input,cached,cache-write,output" in US dollars per
// million tokens. SEYAL_USD_INR is the rupee rate used in the cost report (default 88). The speech
// service for voice notes has its own settings (SEYAL_AI_TRANSCRIBE..., see speech.go).
func FromEnv(get func(string) string) (*Setup, error) {
	s := &Setup{USDtoINR: 88}
	if v := strings.TrimSpace(get("SEYAL_USD_INR")); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil || rate <= 0 {
			return nil, fmt.Errorf("SEYAL_USD_INR: %q is not a positive number", v)
		}
		s.USDtoINR = rate
	}
	for _, p := range Purposes {
		key := "SEYAL_AI_" + strings.ToUpper(string(p))
		spec := strings.TrimSpace(get(key))
		if spec == "" {
			continue
		}
		provider, model, ok := strings.Cut(spec, ":")
		if !ok || provider == "" || model == "" {
			return nil, fmt.Errorf("%s: write it as provider:model, e.g. fake:demo", key)
		}
		providersMu.Lock()
		open, found := providers[provider]
		names := make([]string, 0, len(providers))
		for n := range providers {
			names = append(names, n)
		}
		providersMu.Unlock()
		if !found {
			slices.Sort(names)
			return nil, fmt.Errorf("%s: unknown provider %q (available: %s)", key, provider, strings.Join(names, ", "))
		}
		m, err := open(model, get)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		price, err := parsePrice(get(key + "_PRICE"))
		if err != nil {
			return nil, fmt.Errorf("%s_PRICE: %w", key, err)
		}
		s.Set(p, Choice{Name: spec, Model: m, Price: price})
	}
	if err := speechFromEnv(s, get); err != nil {
		return nil, err
	}
	return s, nil
}

func parsePrice(v string) (Price, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Price{}, nil // unknown: costs show as 0 until a price is set
	}
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return Price{}, fmt.Errorf("write four numbers: input,cached,cache-write,output (US$ per million tokens)")
	}
	nums := make([]float64, 4)
	for i, part := range parts {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || n < 0 {
			return Price{}, fmt.Errorf("%q is not a price", part)
		}
		nums[i] = n
	}
	return Price{Input: nums[0], Cached: nums[1], CacheWrite: nums[2], Output: nums[3]}, nil
}
