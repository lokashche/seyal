// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The limits for other apps (the design's numbers): 20 seconds for a whole call, at most 64 KB of a
// reply read.
const (
	AppTimeout  = 20 * time.Second
	AppMaxReply = 64 << 10
)

// NewAppCaller returns the outgoing caller for other apps: only the hosts in hosts (the server's
// SEYAL_SEND_HOSTS), 20 seconds, 64 KB.
func NewAppCaller(hosts Hosts) *Caller {
	return New(hosts.Allows, AppTimeout, AppMaxReply)
}

// Hosts is the list of hosts the server's admin agreed Seyal may send to.
type Hosts map[string]bool

// ParseHosts reads a comma-separated list of host names ("script.google.com, hook.eu1.make.com").
// Each must be a plain name with a dot in it, not an IP address and not a port; case is ignored.
// An empty list allows nothing.
//
// A wrong entry's error never repeats the entry: an operator may paste a whole webhook address, and
// its path is often a secret. It names the entry by its place and, for a full address, its host only.
func ParseHosts(list string) (Hosts, error) {
	h := Hosts{}
	place := 0
	for _, part := range strings.Split(list, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		place++
		if !hostName(name) {
			hint := ""
			if u, err := url.Parse(name); err == nil && u.Scheme != "" && hostName(u.Hostname()) {
				hint = " It looks like a full address: write only its host, " + u.Hostname() + "."
			}
			return nil, errors.New("send: entry " + strconv.Itoa(place) + " of the list is not a host name " +
				"(write names like hook.example.com, with no https://, no path and no port)." + hint)
		}
		h[name] = true
	}
	return h, nil
}

// hostName reports whether name (lower case) is a plain host name with a dot, not an IP address.
func hostName(name string) bool {
	return plainName(name) && strings.Contains(name, ".") &&
		!strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".") && !isNumeric(name)
}

// isNumeric reports whether name is made only of digits and dots (an IPv4 address, or close to one).
func isNumeric(name string) bool {
	return strings.Trim(name, "0123456789.") == ""
}

// Allows reports whether host (lower case, no port) is on the list.
func (h Hosts) Allows(host string) bool { return h[host] }

// Kind is what an answer means for a send.
type Kind int

const (
	Done  Kind = iota // the other app took it
	Retry             // the other app had a problem (5xx, 429, timeout, no connection): try again later
	Gone              // 410: the address was switched off; pause the target
	Fix               // something in the set-up is wrong (3xx, other 4xx, "ok":false): an admin must fix it
)

// Verdict is a send's outcome with its fixed short reason. Reason is safe to store and show: it is
// made only from a reply code, one of the caller's fixed reasons, or the other app's own short "why"
// (cleaned and cut to 100 characters). It never holds the address or Go's error text.
type Verdict struct {
	Kind   Kind
	Reason string
}

// Fixed reasons Judge adds to the caller's own.
const (
	ReasonOK      = "answered OK"
	ReasonSignIn  = "needs sign-in"            // a redirect to Google's sign-in page
	ReasonUnclear = "answered without an OK"   // ok_json: a 2xx whose body isn't the expected JSON
	ReasonScript  = "script failed: "          // ok_json: "ok":false, then the other app's why
	reasonNoWhy   = "script failed: no reason" // ok_json: "ok":false with no why
)

// signInHost is Google's sign-in page. An Apps Script web app whose sharing or deployment changed
// answers with a redirect there instead of running.
const signInHost = "accounts.google.com"

// maxWhy is the most of the other app's "why" kept, in characters.
const maxWhy = 100

// Judge reads the outcome of one PostRedirect. okJSON is set for a target whose replies are JSON
// with "ok" (an Apps Script web app always answers 200, even when it fails).
func Judge(a Answer, err error, okJSON bool) Verdict {
	if err != nil {
		var f *Failure
		if !errors.As(err, &f) {
			return Verdict{Fix, ReasonFailed}
		}
		switch f.Reason {
		case ReasonTimeout, ReasonConnect, ReasonTLS, ReasonFailed:
			return Verdict{Retry, f.Reason}
		}
		return Verdict{Fix, f.Reason}
	}
	code := "answered " + strconv.Itoa(a.Status)
	switch {
	case a.Status >= 200 && a.Status < 300:
		if !okJSON {
			return Verdict{Done, ReasonOK}
		}
		var reply struct {
			OK  *bool  `json:"ok"`
			Why string `json:"why"`
		}
		if json.Unmarshal(a.Body, &reply) != nil || reply.OK == nil {
			return Verdict{Retry, ReasonUnclear}
		}
		if *reply.OK {
			return Verdict{Done, ReasonOK}
		}
		if why := cleanWhy(reply.Why); why != "" {
			return Verdict{Fix, ReasonScript + why}
		}
		return Verdict{Fix, reasonNoWhy}
	case isRedirect(a.Status) && a.RedirectHost == signInHost:
		return Verdict{Fix, ReasonSignIn}
	case a.Status == http.StatusGone:
		return Verdict{Gone, code}
	case a.Status == http.StatusTooManyRequests || a.Status >= 500:
		return Verdict{Retry, code}
	}
	return Verdict{Fix, code}
}

// cleanWhy keeps the other app's reason short and plain: control characters and hidden format
// marks (marks that change the direction of text, which could make a reason read backwards, and
// zero-width spaces) become spaces, runs of spaces become one, and it is cut to maxWhy characters.
func cleanWhy(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}), " ")
	if utf8.RuneCountInString(s) > maxWhy {
		s = string([]rune(s)[:maxWhy])
	}
	return s
}
