// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http/httptest"
	"testing"
)

// TestClientIP: behind our proxy, the client is the last address the proxy added; anything the client
// put in X-Forwarded-For itself is ignored. Without a proxy, the header is never trusted.
func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/t/demo/login", nil)
	r.RemoteAddr = "172.18.0.5:41000" // the proxy, inside the container network
	r.Header.Add("X-Forwarded-For", "1.2.3.4, 203.0.113.9")

	if got := (&Server{proxied: true}).clientIP(r); got != "203.0.113.9" {
		t.Errorf("behind the proxy = %q, want the address the proxy saw", got)
	}
	if got := (&Server{}).clientIP(r); got != "172.18.0.5" {
		t.Errorf("without a proxy = %q, want the connection's own address", got)
	}
	r.Header.Del("X-Forwarded-For")
	if got := (&Server{proxied: true}).clientIP(r); got != "172.18.0.5" {
		t.Errorf("behind the proxy, no header = %q", got)
	}
}
