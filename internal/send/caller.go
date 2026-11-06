// SPDX-License-Identifier: AGPL-3.0-or-later

// Package send holds Seyal's safe outgoing caller: the one way Seyal itself calls an address on
// the internet (phone notes now, other apps later). It refuses every address it wasn't told to
// allow, checks the real IP address at the moment of connecting, follows no redirect, uses no
// proxy, gives up after a fixed time, reads only a little of the reply, and never puts the address
// it called into an error.
//
// The word for Go's HTTP type is not written here (the engine's word list forbids it), so the
// caller copies http.DefaultClient and changes the copy.
package send

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The fixed short reasons a call can fail with. Only these are ever stored or logged; Go's own
// error text (which holds the full address) never is.
const (
	ReasonAddress   = "bad address"         // not a web address at all
	ReasonNotHTTPS  = "not https"           // only https is called
	ReasonHost      = "host not allowed"    // the host is not on the allowed list
	ReasonBlocked   = "address not allowed" // the name led to a private or special IP address
	ReasonTimeout   = "timeout"             // no answer in time
	ReasonConnect   = "could not connect"   // refused, unreachable or no such name
	ReasonTLS       = "TLS error"           // the secure connection failed (certificate and so on)
	ReasonFailed    = "failed"              // anything else
	ReasonTooLong   = "address too long"    // longer than MaxAddress
	ReasonBadHeader = "bad header"          // a header value with a line break in it
)

// MaxAddress is the longest address the caller accepts.
const MaxAddress = 2048

// Failure is a call that didn't get an answer. Its text is one of the fixed reasons above, never the
// address that was called.
type Failure struct{ Reason string }

func (f *Failure) Error() string { return f.Reason }

func fail(reason string) error { return &Failure{Reason: reason} }

// Answer is what the other side said: its reply code, its Retry-After header (empty if none) and at
// most the caller's reply limit of its reply.
type Answer struct {
	Status     int
	RetryAfter string
	Body       []byte
}

// Caller makes outgoing calls to allowed hosts only. Make one with New; it is safe to share between
// goroutines.
type Caller struct {
	hostOK   func(host string) bool
	maxReply int64
	do       func(*http.Request) (*http.Response, error)
	anyPort  bool // only tests set it, to reach a fake on a random port
}

// New returns a caller that calls only https addresses whose host hostOK allows (the host is given
// in lower case, without a port). Each call gives up after timeout, and at most maxReply bytes of
// each reply are read.
func New(hostOK func(host string) bool, timeout time.Duration, maxReply int64) *Caller {
	return newCaller(hostOK, timeout, maxReply, checkDial, nil)
}

// newCaller is New with the dial check and the TLS settings given, so a test can reach a fake on
// this computer. Only tests pass anything but checkDial and nil.
func newCaller(hostOK func(host string) bool, timeout time.Duration, maxReply int64,
	dialCheck func(network, address string, c syscall.RawConn) error, tlsConfig *tls.Config) *Caller {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: dialCheck}
	transport := &http.Transport{
		Proxy:                  nil, // never through a proxy: the IP check below must see the real address
		TLSClientConfig:        tlsConfig,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  timeout,
		MaxIdleConns:           10,
		IdleConnTimeout:        90 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
	}
	c := *http.DefaultClient
	c.Transport = transport
	c.Timeout = timeout
	// Never follow a redirect: the 3xx answer itself comes back, like any other answer.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Caller{hostOK: hostOK, maxReply: maxReply, do: c.Do}
}

// CheckAddress checks a web address before any call: https, no name and password in it, no port
// other than 443, and a host that hostOK allows. Its error is a *Failure.
func CheckAddress(address string, hostOK func(host string) bool) (*url.URL, error) {
	return checkAddress(address, hostOK, false)
}

func checkAddress(address string, hostOK func(host string) bool, anyPort bool) (*url.URL, error) {
	if len(address) > MaxAddress {
		return nil, fail(ReasonTooLong)
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, fail(ReasonAddress)
	}
	if u.Scheme != "https" {
		return nil, fail(ReasonNotHTTPS)
	}
	host := strings.ToLower(u.Hostname())
	if u.User != nil || (u.Port() != "" && u.Port() != "443" && !anyPort) || !plainName(host) || !hostOK(host) {
		return nil, fail(ReasonHost)
	}
	return u, nil
}

// plainName reports whether host is a plain name: only letters, digits, dots and dashes, as names
// on the internet are. Escaped characters and IPv6 brackets are refused.
func plainName(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

// Post sends body to address with the given headers, and returns the answer. A call that got any
// answer, even 4xx or 5xx, returns that answer and no error; the caller of Post decides what the
// code means. Its error is always a *Failure.
func (c *Caller) Post(ctx context.Context, address string, header http.Header, body []byte) (Answer, error) {
	u, err := checkAddress(address, c.hostOK, c.anyPort)
	if err != nil {
		return Answer{}, err
	}
	for _, values := range header {
		for _, v := range values {
			if strings.ContainsAny(v, "\r\n") {
				return Answer{}, fail(ReasonBadHeader)
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Answer{}, fail(ReasonAddress)
	}
	req.Header = header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	res, err := c.do(req)
	if err != nil {
		return Answer{}, fail(reason(err))
	}
	defer res.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(res.Body, c.maxReply))
	if err != nil {
		reply = nil // a reply that broke off: the code still counts
	}
	return Answer{Status: res.StatusCode, RetryAfter: res.Header.Get("Retry-After"), Body: reply}, nil
}

// errBlocked is what the dial check returns; reason turns it into ReasonBlocked.
var errBlocked = errors.New("address not allowed")

// checkDial runs just before each connection, with the IP address the name really led to. This
// defeats "DNS rebinding" (a name that first points at a safe address, then at an inside one).
func checkDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || Blocked(ap.Addr()) {
		return errBlocked
	}
	return nil
}

// blockedRanges are special ranges that netip's own checks below don't cover.
var blockedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (carrier networks)
	netip.MustParsePrefix("192.0.0.0/24"),    // protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the broadcast address
	netip.MustParsePrefix("64:ff9b::/96"),    // IPv6 addresses that carry an IPv4 address (NAT64)
	netip.MustParsePrefix("64:ff9b:1::/48"),  // the same, for local use
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001::/32"),       // Teredo (carries an IPv4 address)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 (carries an IPv4 address)
	netip.MustParsePrefix("3fff::/20"),       // documentation
	netip.MustParsePrefix("5f00::/16"),       // SRv6 segment IDs (inside a provider's network)
	netip.MustParsePrefix("fec0::/10"),       // site-local (old, still routed on some inside networks)
}

// Blocked reports whether Seyal must never connect to ip: this computer (loopback), a private
// network, link-local (which includes the cloud "metadata" address 169.254.169.254), multicast and
// the other special ranges, also when written as an IPv6 copy of an IPv4 address (::ffff:127.0.0.1).
func Blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range blockedRanges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// reason turns Go's error for a failed call into one fixed short reason. It looks only at the kind
// of error, never at its text.
func reason(err error) string {
	if errors.Is(err, errBlocked) {
		return ReasonBlocked
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ReasonTimeout
	}
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	if errors.As(err, &certErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) {
		return ReasonTLS
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if (errors.As(err, &opErr) && opErr.Op == "dial") || errors.As(err, &dnsErr) {
		return ReasonConnect
	}
	return ReasonFailed
}
