// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"io"
	"log"
	"slices"
	"sync"
	"testing"
	"time"

	"seyal/internal/outbox"
)

// TestSecureCookies: HTTPS-only cookies unless switched off or listening only on this computer.
func TestSecureCookies(t *testing.T) {
	for _, tt := range []struct {
		setting, addr string
		want          bool
	}{
		{"", "", false},                  // default address: this computer only
		{"", "127.0.0.1:8080", false},    // development
		{"", "localhost:8080", false},    // development
		{"", ":8080", true},              // in a container or on a server: secure
		{"", "0.0.0.0:8080", true},       // listening for others: secure
		{"false", ":8080", false},        // switched off on purpose
		{"true", "127.0.0.1:8080", true}, // switched on on purpose
	} {
		if got := secureCookies(tt.setting, tt.addr); got != tt.want {
			t.Errorf("secureCookies(%q, %q) = %v, want %v", tt.setting, tt.addr, got, tt.want)
		}
	}
}

// TestLoops: the phone-note reader and the sender to other apps run whether or not the chaser does
// (SEYAL_CHASE_EVERY=off), and all stop when the server stops. Nothing here touches a database: the server has stopped already.
func TestLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for every, want := range map[time.Duration][]string{
		0:               {"phone-note reader", "sender to other apps"},
		5 * time.Minute: {"chaser", "phone-note reader", "sender to other apps"},
	} {
		var wg sync.WaitGroup
		if got := loops(ctx, &wg, nil, io.Discard, every, newReader(nil, io.Discard, nil),
			outbox.New(nil, log.New(io.Discard, "", 0), false, nil, nil)); !slices.Equal(got, want) {
			t.Errorf("with the chaser every %s: started %v, want %v", every, got, want)
		}
		wg.Wait()
	}
}
