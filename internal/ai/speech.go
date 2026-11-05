// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Speech to text: a voice note turned into words, which the person then checks and sends like typed
// text (D34, D66). A speech service is chosen like the chat models, as SEYAL_AI_TRANSCRIBE=
// provider:model, and every call is recorded in AI usage. It is off until that is set.

// Transcribe is the purpose recorded for speech-to-text calls in AI usage.
const Transcribe Purpose = "transcribe"

// Audio is one recording to turn into words.
type Audio struct {
	Data        []byte
	ContentType string // the plain type, e.g. "audio/webm"
	// Language is a hint such as "ta" or "en". Empty lets the service work it out, which suits
	// people who mix languages in one sentence.
	Language string
	// Words are names people are likely to say, to help the service spell them. Keep it short:
	// services read only the first couple of hundred tokens.
	Words string
}

// Transcript is what a speech service heard.
type Transcript struct {
	Text     string
	Language string  // the language it heard, if the service says
	Seconds  float64 // the length of the recording, if the service says; used for the cost
}

// Transcriber is one speech service's model. Adding a service means implementing this and
// registering it with RegisterSpeech.
type Transcriber interface {
	Transcribe(ctx context.Context, a Audio) (Transcript, error)
}

// Speech is the speech service chosen in the configuration.
type Speech struct {
	Name     string // "provider:model", as recorded
	Model    Transcriber
	PerHour  float64 // US$ per hour of audio; 0 = unknown, costs show as 0
	Language string  // optional hint for every recording; "" = the service works it out
	// DayMinutes and LoginDayMinutes are the most minutes of audio sent to the service in any 24
	// hours for one business and for one login (the server counts them); 0 = the defaults.
	DayMinutes, LoginDayMinutes int
}

// The most minutes of audio sent to the speech service in any 24 hours, unless the configuration
// says otherwise: for one business (4 hours) and for one login (1 hour). The service is paid per
// minute, so this caps what one business, or one login (or a stolen login cookie), can spend.
const (
	DefaultDayMinutes      = 240
	DefaultLoginDayMinutes = 60
)

// SetSpeech chooses the speech service.
func (s *Setup) SetSpeech(sp Speech) { s.speech = &sp }

// Speech returns the speech service, if one is set.
func (s *Setup) Speech() (Speech, bool) {
	if s == nil || s.speech == nil {
		return Speech{}, false
	}
	return *s.speech, true
}

// MaxAudio is the largest recording sent to a speech service (10 MB). The composer stops recording
// after five minutes, which is about 5 MB; a larger file didn't come from it. The services' basic
// plans take up to 25 MB. The size doesn't limit the cost (services charge by length, and a small
// file can hold hours): MaxSeconds does that (audiolength.go).
const MaxAudio = 10 << 20

// ErrTooLarge means the recording is longer or larger than Seyal sends to a speech service.
var ErrTooLarge = errors.New("the recording is too long to turn into words")

// ErrNoWords means the service answered but heard no words (silence, or noise).
var ErrNoWords = errors.New("no words were heard")

// SentSeconds is the length of a recording that Transcribe would send to the service, as Seyal
// measures it; 0 when it wouldn't send it (empty, too large, too long, or not measurable). The server
// counts it before the call, to keep each business and login within its minutes a day.
func SentSeconds(data []byte) float64 {
	if len(data) == 0 || len(data) > MaxAudio {
		return 0
	}
	secs, err := audioSeconds(data)
	if err != nil || secs > MaxSeconds {
		return 0
	}
	return secs
}

// Called is one AI call, ready to be recorded in AI usage.
type Called struct {
	Purpose Purpose
	Model   string
	Link    Link
	Usage   Usage
	CostUSD float64
	Took    time.Duration
	Err     error
}

// Transcribe sends a recording to the speech service and returns what it heard, plus the call to
// record. It returns ErrOff when no service is set. It doesn't touch the database, so no database
// transaction waits while the audio travels; the caller records the call with Called.Record, also
// when the service failed.
//
// Only recordings Seyal can measure, up to MaxSeconds, are sent (ErrUnmeasured, ErrTooLarge). The
// cost uses the length the service gives. When it gives none (its plain answer has none, or the call
// failed or was given up on after the recording reached it, when the service may still charge), the
// cost uses the length Seyal measured, so AI usage doesn't show such calls as free.
func (s *Setup) Transcribe(ctx context.Context, a Audio, link Link) (Transcript, Called, error) {
	sp, ok := s.Speech()
	if !ok {
		return Transcript{}, Called{}, ErrOff
	}
	if a.Language == "" {
		a.Language = sp.Language
	}
	call := Called{Purpose: Transcribe, Model: sp.Name, Link: link}
	var tr Transcript
	var err error
	var measured float64 // the recording's length, as Seyal measured it; 0 = not sent
	start := time.Now()
	switch {
	case len(a.Data) == 0:
		err = errors.New("the recording is empty")
	case len(a.Data) > MaxAudio:
		err = ErrTooLarge
	default:
		if measured, err = audioSeconds(a.Data); err == nil && measured > MaxSeconds {
			measured, err = 0, ErrTooLarge
		}
		if err == nil {
			tr, err = sp.Model.Transcribe(ctx, a)
		}
	}
	call.Took = time.Since(start)
	tr.Text = strings.TrimSpace(tr.Text)
	if err == nil && tr.Text == "" {
		err = ErrNoWords
	}
	seconds := tr.Seconds
	if seconds <= 0 {
		seconds = measured
	}
	if measured > 0 && tr.Seconds > measuredFar(measured) {
		// The service heard a much longer recording than Seyal measured: a file made to be measured
		// short. The cost above is the service's; the log says which login sent it.
		slog.Warn("speech service heard a longer recording than Seyal measured", "service", sp.Name,
			"measured_seconds", measured, "service_seconds", tr.Seconds, "person", link.PersonID)
	}
	call.CostUSD = seconds / 3600 * sp.PerHour
	if err != nil {
		call.Err = err
		return Transcript{}, call, fmt.Errorf("ai: %s: %w", sp.Name, err)
	}
	return tr, call, nil
}

// measuredFar is the length a service may report for a recording Seyal measured as measured
// seconds before Seyal logs a warning: a fifth more, plus 5 seconds (services round, and count
// the codec's start-up sound).
func measuredFar(measured float64) float64 { return measured*1.2 + 5 }

// Record writes the call into AI usage. tx must come from db.WithTenant.
func (c Called) Record(ctx context.Context, tx pgx.Tx) error {
	msg := ""
	if c.Err != nil {
		msg = c.Err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ai_calls (tenant_id, purpose, model, person_id, work_item_id, input_tokens, cached_tokens,
			cache_write_tokens, output_tokens, cost_usd, duration_ms, ok, error)
		VALUES (current_tenant(), $1, $2, nullif($3::text, '')::uuid, nullif($4::text, '')::uuid, $5, $6, $7, $8,
			$9, $10, $11, $12)`,
		string(c.Purpose), c.Model, c.Link.PersonID, c.Link.ItemID, c.Usage.Input, c.Usage.Cached, c.Usage.CacheWrite,
		c.Usage.Output, c.CostUSD, c.Took.Milliseconds(), c.Err == nil, strings.ToValidUTF8(msg, "")); err != nil {
		return fmt.Errorf("ai: record call: %w", err)
	}
	return nil
}

// Speech services by provider name: "fake" (tests and demos) and the OpenAI-style services in
// whisper.go.
var (
	speechMu        sync.Mutex
	speechProviders = map[string]func(model string, get func(string) string) (Transcriber, error){
		"fake": func(string, func(string) string) (Transcriber, error) { return &FakeSpeech{}, nil },
	}
)

// RegisterSpeech makes a speech service available to the configuration.
func RegisterSpeech(name string, open func(model string, get func(string) string) (Transcriber, error)) {
	speechMu.Lock()
	defer speechMu.Unlock()
	speechProviders[name] = open
}

var languageHint = regexp.MustCompile(`^[a-z]{2,3}$`)

// speechFromEnv reads SEYAL_AI_TRANSCRIBE ("provider:model", empty = off),
// SEYAL_AI_TRANSCRIBE_PRICE (US$ per hour of audio), SEYAL_AI_TRANSCRIBE_LANGUAGE (an optional
// hint such as "ta"; empty = the service works out the language, also for mixed speech), and
// SEYAL_AI_TRANSCRIBE_DAY_MINUTES and SEYAL_AI_TRANSCRIBE_LOGIN_DAY_MINUTES (the most minutes of
// audio in any 24 hours for one business and for one login; empty = the defaults).
func speechFromEnv(s *Setup, get func(string) string) error {
	const key = "SEYAL_AI_TRANSCRIBE"
	spec := strings.TrimSpace(get(key))
	if spec == "" {
		return nil
	}
	provider, model, ok := strings.Cut(spec, ":")
	if !ok || provider == "" || model == "" {
		return fmt.Errorf("%s: write it as provider:model, e.g. fake:demo", key)
	}
	speechMu.Lock()
	open, found := speechProviders[provider]
	names := make([]string, 0, len(speechProviders))
	for n := range speechProviders {
		names = append(names, n)
	}
	speechMu.Unlock()
	if !found {
		slices.Sort(names)
		return fmt.Errorf("%s: unknown provider %q (available: %s)", key, provider, strings.Join(names, ", "))
	}
	m, err := open(model, get)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	sp := Speech{Name: spec, Model: m}
	if v := strings.TrimSpace(get(key + "_PRICE")); v != "" {
		if sp.PerHour, err = strconv.ParseFloat(v, 64); err != nil || sp.PerHour < 0 {
			return fmt.Errorf("%s_PRICE: %q is not a price (US$ per hour of audio)", key, v)
		}
	}
	sp.Language = strings.ToLower(strings.TrimSpace(get(key + "_LANGUAGE")))
	if sp.Language != "" && !languageHint.MatchString(sp.Language) {
		return fmt.Errorf("%s_LANGUAGE: %q is not a language code such as ta or en (leave it empty to detect)", key, sp.Language)
	}
	for _, limit := range []struct {
		name string
		to   *int
		def  int
	}{{"_DAY_MINUTES", &sp.DayMinutes, DefaultDayMinutes}, {"_LOGIN_DAY_MINUTES", &sp.LoginDayMinutes, DefaultLoginDayMinutes}} {
		*limit.to = limit.def
		if v := strings.TrimSpace(get(key + limit.name)); v != "" {
			if *limit.to, err = strconv.Atoi(v); err != nil || *limit.to < 1 {
				return fmt.Errorf("%s%s: %q is not a number of minutes (1 or more)", key, limit.name, v)
			}
		}
	}
	s.SetSpeech(sp)
	return nil
}

// FakeSpeech is a stand-in speech service for tests and demos. It hears nothing: it returns the
// prepared words, so the whole flow can be tried without a real service, a key or any cost.
type FakeSpeech struct {
	// Answer returns the prepared transcript. If nil, FakeSpeech returns a fixed demo sentence.
	Answer func(a Audio) (Transcript, error)

	mu   sync.Mutex
	seen []Audio
}

// FakeWords is what FakeSpeech hears when no answer is prepared.
const FakeWords = "This is a demo transcript, not your words. Correct it, then press Send."

// Transcribe returns the prepared transcript.
func (f *FakeSpeech) Transcribe(ctx context.Context, a Audio) (Transcript, error) {
	if err := ctx.Err(); err != nil {
		return Transcript{}, err
	}
	f.mu.Lock()
	f.seen = append(f.seen, a)
	f.mu.Unlock()
	if f.Answer == nil {
		return Transcript{Text: FakeWords, Seconds: 5}, nil
	}
	return f.Answer(a)
}

// Seen returns the recordings the fake received, so tests can check what Seyal sent.
func (f *FakeSpeech) Seen() []Audio {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Audio(nil), f.seen...)
}
