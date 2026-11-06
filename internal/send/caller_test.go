// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.8.8", "::1", "::ffff:127.0.0.1", // this computer, also as an IPv6 copy
		"10.0.0.1", "172.16.5.5", "192.168.1.10", "fd00::1", "::ffff:192.168.1.10", // office networks
		"169.254.169.254", "fe80::1", // link-local, with the cloud "metadata" address
		"100.64.0.1", "0.0.0.0", "::", "255.255.255.255", "224.0.0.1", "ff02::1",
		"192.0.2.1", "198.18.0.1", "240.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:db8::1",
		// IPv6 copies of addresses only blockedRanges catches: netip's own IsLoopback/IsPrivate
		// unwrap a copy themselves, but Prefix.Contains does not, so these need ip.Unmap().
		"::ffff:100.64.0.1", "::ffff:192.0.2.1", "::ffff:240.0.0.1",
		// IPv6 ranges netip calls "global unicast" and not private.
		"fec0::1", "fec0::a:1", "feff::1", "3fff::1", "5f00::1",
	}
	for _, s := range blocked {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("Blocked(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"142.250.183.10", "17.253.144.10", "2a00:1450:4001:82a::200a"} {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("Blocked(%s) = true, want false (a public address)", s)
		}
	}
	if !Blocked(netip.Addr{}) {
		t.Error("an empty address is not blocked")
	}
}

func TestCheckAddress(t *testing.T) {
	only := func(h string) bool { return h == "hooks.kaveri.example" }
	if _, err := CheckAddress("https://Hooks.Kaveri.example/in?x=1", only); err != nil {
		t.Errorf("allowed host refused: %v", err)
	}
	for address, want := range map[string]string{
		"http://hooks.kaveri.example/in":                                  ReasonNotHTTPS,
		"ftp://hooks.kaveri.example/in":                                   ReasonNotHTTPS,
		"https://other.kaveri.example/in":                                 ReasonHost,
		"https://hooks.kaveri.example:8443/in":                            ReasonHost,
		"https://u:p@hooks.kaveri.example/in":                             ReasonHost,
		"https://hooks%2ekaveri.example/in":                               ReasonAddress,
		"https:///in":                                                     ReasonAddress,
		"hooks.kaveri.example/in":                                         ReasonAddress,
		"https://hooks.kaveri.example/" + strings.Repeat("a", MaxAddress): ReasonTooLong,
	} {
		_, err := CheckAddress(address, only)
		var f *Failure
		if !errors.As(err, &f) || f.Reason != want {
			t.Errorf("CheckAddress(%q) = %v, want %q", address, err, want)
		}
	}
}

// A fake on this computer, with a marker word in its address. The marker must never show in an error.
const marker = "markerK9V4"

func fake(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.URL + "/s/" + marker + "/exec"
}

func TestDialCheckRefusesThisComputer(t *testing.T) {
	called := false
	_, address := fake(t, func(w http.ResponseWriter, _ *http.Request) { called = true })
	// Even when the host list lets the name through, the dial check sees the real IP (127.0.0.1)
	// and refuses to connect.
	c := New(func(string) bool { return true }, 5*time.Second, 4096)
	c.anyPort = true
	_, err := c.Post(context.Background(), strings.Replace(address, "127.0.0.1", "localhost", 1), nil, nil)
	var f *Failure
	if !errors.As(err, &f) || f.Reason != ReasonBlocked {
		t.Errorf("Post to this computer = %v, want %q", err, ReasonBlocked)
	}
	if err != nil && strings.Contains(err.Error(), marker) {
		t.Errorf("the error shows the address: %v", err)
	}
	if called {
		t.Error("the fake on this computer was reached")
	}
}

// allowAll lets a test reach its fake on this computer; only tests use it.
func allowAll(string, string, syscall.RawConn) error { return nil }

func testCaller(srv *httptest.Server, timeout time.Duration, maxReply int64) *Caller {
	cfg := &tls.Config{RootCAs: nil}
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		cfg = tr.TLSClientConfig.Clone()
	}
	c := newCaller(func(h string) bool { return h == "127.0.0.1" }, timeout, maxReply, allowAll, cfg)
	c.anyPort = true
	return c
}

func TestPost(t *testing.T) {
	var got []byte
	var gotHeader http.Header
	srv, address := fake(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		gotHeader = r.Header
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(strings.Repeat("x", 10000)))
	})
	c := testCaller(srv, 5*time.Second, 100)
	ans, err := c.Post(context.Background(), address, http.Header{"Ttl": {"60"}}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if ans.Status != http.StatusTooManyRequests || ans.RetryAfter != "120" || len(ans.Body) != 100 {
		t.Errorf("answer = %d, %q, %d bytes; want 429, 120, 100 bytes (the reply limit)", ans.Status, ans.RetryAfter, len(ans.Body))
	}
	if string(got) != "hello" || gotHeader.Get("TTL") != "60" {
		t.Errorf("the fake got %q and TTL %q", got, gotHeader.Get("TTL"))
	}

	// A header with a line break is refused before any call.
	if _, err := c.Post(context.Background(), address, http.Header{"Topic": {"a\r\nX-Evil: 1"}}, nil); err == nil {
		t.Error("a header with a line break was sent")
	}
}

func TestPostFollowsNoRedirect(t *testing.T) {
	followed := false
	var srv *httptest.Server
	srv, address := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusFound)
	})
	ans, err := testCaller(srv, 5*time.Second, 100).Post(context.Background(), address, nil, nil)
	if err != nil || ans.Status != http.StatusFound {
		t.Errorf("answer = %+v, %v; want the 302 itself", ans, err)
	}
	if followed {
		t.Error("the redirect was followed")
	}
}

func TestPostErrorsHoldNoAddress(t *testing.T) {
	block := make(chan struct{})
	srv, address := fake(t, func(http.ResponseWriter, *http.Request) { <-block })
	defer close(block)
	_, err := testCaller(srv, 200*time.Millisecond, 100).Post(context.Background(), address, nil, nil)
	var f *Failure
	if !errors.As(err, &f) || f.Reason != ReasonTimeout {
		t.Errorf("a slow fake: %v, want %q", err, ReasonTimeout)
	}
	if err != nil && strings.Contains(err.Error(), marker) {
		t.Errorf("the error shows the address: %v", err)
	}

	// A server whose certificate this caller doesn't trust: a TLS error, again without the address.
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	c := newCaller(func(h string) bool { return h == "127.0.0.1" }, 5*time.Second, 100, allowAll, nil)
	c.anyPort = true
	_, err = c.Post(context.Background(), other.URL+"/s/"+marker, nil, nil)
	if !errors.As(err, &f) || f.Reason != ReasonTLS {
		t.Errorf("an untrusted certificate: %v, want %q", err, ReasonTLS)
	}
	if err != nil && strings.Contains(err.Error(), marker) {
		t.Errorf("the error shows the address: %v", err)
	}
}
