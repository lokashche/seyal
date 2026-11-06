// SPDX-License-Identifier: AGPL-3.0-or-later

package chase

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
)

// TestEvery: SEYAL_CHASE_EVERY is 5 minutes when empty, off when "off", and never under a minute.
func TestEvery(t *testing.T) {
	for _, tt := range []struct {
		setting string
		want    time.Duration
		ok      bool
	}{
		{"", 5 * time.Minute, true},
		{"off", 0, true},
		{"1m", time.Minute, true},
		{"1h", time.Hour, true},
		{"90s", 90 * time.Second, true},
		{"59s", 0, false},
		{"0", 0, false},
		{"-5m", 0, false},
		{"five minutes", 0, false},
		{"OFF", 0, false},
	} {
		got, err := Every(tt.setting)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("Every(%q) = %v, %v; want %v, ok %v", tt.setting, got, err, tt.want, tt.ok)
		}
	}
	for d, want := range map[time.Duration]string{
		5 * time.Minute:  "Seyal checks for work to chase every 5 minutes.",
		time.Hour:        "Seyal checks for work to chase every 1 hour.",
		time.Minute:      "Seyal checks for work to chase every 1 minute.",
		90 * time.Second: "Seyal checks for work to chase every 90 seconds.",
		0:                "Seyal doesn't chase work on this server (SEYAL_CHASE_EVERY=off).",
	} {
		if got := Says(d); got != want {
			t.Errorf("Says(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestClockChecks: a round does nothing when the app's clock is more than 2 minutes away from the
// database's, or more than 2 minutes behind a business's last ledger event.
func TestClockChecks(t *testing.T) {
	database := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for app, ok := range map[time.Time]bool{
		database:                       true,
		database.Add(2 * time.Minute):  true,
		database.Add(-2 * time.Minute): true,
		database.Add(3 * time.Minute):  false,
		database.Add(-3 * time.Minute): false,
		database.Add(48 * time.Hour):   false, // a clock that jumps two days can't make work look late
	} {
		if err := clocksAgree(app, database); (err == nil) != ok {
			t.Errorf("clocksAgree(app %s, database %s) = %v, want ok %v", app, database, err, ok)
		}
	}
	last := database
	for app, ok := range map[time.Time]bool{
		last.Add(time.Hour):        true, // later than the last event: normal
		last.Add(-time.Minute):     true, // a little behind: within the slack
		last.Add(-3 * time.Minute): false,
		last.Add(-24 * time.Hour):  false, // a clock that went back a day can't cause a second reminder
	} {
		if err := notBefore(app, last); (err == nil) != ok {
			t.Errorf("notBefore(app %s, last %s) = %v, want ok %v", app, last, err, ok)
		}
	}
}

// noDatabase is a database that can't be reached: every round fails at its first step.
type noDatabase struct{}

func (noDatabase) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("no database here") }

// lockedBuffer is a log that two goroutines may use at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLoopStopsOnCancel: the loop runs rounds until its context ends, then returns at once, also while
// it waits for the first round. A round whose database is down is logged and the next one tries again.
func TestLoopStopsOnCancel(t *testing.T) {
	var out lockedBuffer
	c := New(noDatabase{}, log.New(&out, "", 0))

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		c.Run(ctx, 5*time.Millisecond, 0)
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(out.String(), "can't reach the database") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than 3 rounds in 5 s; log: %q", out.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop didn't stop after its context was cancelled")
	}

	// Cancelled while waiting for the first round (a minute away): it returns without a round.
	out = lockedBuffer{}
	ctx, cancel = context.WithCancel(context.Background())
	stopped = make(chan struct{})
	go func() {
		c.Run(ctx, time.Hour, time.Minute)
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop didn't stop while waiting for its first round")
	}
	if out.String() != "" {
		t.Errorf("a round ran before the first wait was over: %q", out.String())
	}
}

// TestAsIfRefusesBadDays: --as-if needs a day written 2026-11-09, and only the demo business, before
// anything is read.
func TestAsIfRefusesBadDays(t *testing.T) {
	c := New(noDatabase{}, log.New(&bytes.Buffer{}, "", 0))
	if _, err := c.AsIf(context.Background(), "acme", "2026-11-09"); err == nil || !strings.Contains(err.Error(), "only the demo business") {
		t.Errorf("another business = %v, want it refused", err)
	}
	for _, day := range []string{"9 Nov", "2026-11-31", "tomorrow", ""} {
		if _, err := c.asIf(context.Background(), db.Tenant{ID: "not-read", Slug: DemoSlug}, day); err == nil {
			t.Errorf("day %q was taken", day)
		}
	}
}
