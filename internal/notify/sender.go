// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"seyal/internal/auth"
	"seyal/internal/push"
	"seyal/internal/send"
)

// The phone-note sender (Phase 4, part 4a-5, D90). Deliver (deliver.go) makes one phone note per
// person and posts it, while that person's notices are locked, to each of their devices at once,
// through the device's push service. What each push service answers decides what happens next:
//
//   - 2xx: it took the note (not proof that the phone showed it). One device taking it is enough: the
//     notices are marked sent, and nothing is posted again.
//   - 401 or 403: the push service doesn't accept Seyal's key for that device (for example after the
//     key changed). The device ends at the end of the business's pass, unless two or more of the
//     pass's people had a device say so and they are more than half of the people posted to (people,
//     not devices, so one login's own devices can't hold it), or that device's own push service
//     (pushFamily) took no note since the server started: then Seyal logs "check the SEYAL_PUSH keys"
//     and doesn't end those devices.
//   - 404 or 410: the device push address is gone. The device ends at once.
//   - 429, 5xx, or no answer (a timeout, no connection): the push service had a problem. The note
//     waits and is tried again after 1, 5 and 30 minutes (longer if the push service asks, with
//     Retry-After, up to MaxAsked), then it is given up.
//   - 400 and the rest: something in the note or the set-up is wrong. It is logged, and that answer
//     alone never brings a retry. (If another of the person's devices asks for a retry, the next try
//     goes to all of their devices again, this one too.)
//
// The notices are marked first; what the answers mean for the device rows (last_ok_at, ending a
// device) is saved after that, in a short transaction of its own (devicesAnswered), so a failure
// there never undoes a mark and never posts a note twice.
//
// The log never holds a device push address: only reply codes and the fixed short reasons of the
// safe outgoing caller (internal/send).

// Pusher posts one phone note to one device's push service. *push.Sender does it: it locks the note
// for that device (RFC 8291) and signs Seyal's token (RFC 8292). Tests use a fake push service, so no
// test ever calls a real one.
type Pusher interface {
	Send(ctx context.Context, d push.Device, m push.Message) (send.Answer, error)
}

// MaxTries is how many times one phone note is posted before it is given up: the first try, then
// after each of RetryWaits.
const MaxTries = 4

// RetryWaits are the waits after the first, second and third try that no device took.
var RetryWaits = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// What a push service's answer means for a phone note and its device.
type outcome int

const (
	took     outcome = iota // 2xx
	tryAgain                // 429, 5xx, no answer: try the note again later
	badKey                  // 401, 403: the push service doesn't accept Seyal's key for this device
	gone                    // 404, 410: the device push address is gone
	noRetry                 // 400, another 4xx, 3xx, or a note or address Seyal can't post
)

// answer is what one device's push service said.
type answer struct {
	outcome outcome
	wait    time.Duration // tryAgain: wait at least this long (the push service's Retry-After)
	why     string        // for the log: "answered 503", or the caller's fixed short reason
	replied bool          // the push service answered with a reply code
}

// classify reads one push service's answer. Only the reply code, Retry-After and the caller's fixed
// reason are looked at; an error's own text is never kept (it could hold the address).
func classify(a send.Answer, err error, now time.Time) answer {
	if err != nil {
		var f *send.Failure
		if !errors.As(err, &f) {
			return answer{outcome: noRetry, why: "could not lock or post it"}
		}
		switch f.Reason {
		case send.ReasonTimeout, send.ReasonConnect, send.ReasonTLS, send.ReasonFailed:
			return answer{outcome: tryAgain, why: f.Reason}
		}
		return answer{outcome: noRetry, why: f.Reason}
	}
	why := "answered " + strconv.Itoa(a.Status)
	switch {
	case a.Status >= 200 && a.Status < 300:
		return answer{outcome: took, why: why, replied: true}
	case a.Status == http.StatusUnauthorized || a.Status == http.StatusForbidden:
		return answer{outcome: badKey, why: why, replied: true}
	case a.Status == http.StatusNotFound || a.Status == http.StatusGone:
		return answer{outcome: gone, why: why, replied: true}
	case a.Status == http.StatusTooManyRequests || a.Status >= 500:
		return answer{outcome: tryAgain, wait: retryAfter(a.RetryAfter, now), why: why, replied: true}
	}
	return answer{outcome: noRetry, why: why, replied: true}
}

// MaxAsked is the longest wait a push service's Retry-After gets: the longest of RetryWaits. A longer
// ask (a date far ahead, a huge number of seconds) waits only this long, so every note still ends
// (sent, failed or stale) and a huge number can't overflow.
const MaxAsked = 30 * time.Minute

// retryAfter reads a Retry-After header: a number of seconds, or a date, at most MaxAsked. Anything
// else is 0.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		if s > int(MaxAsked/time.Second) {
			return MaxAsked // checked before multiplying, so it can't overflow
		}
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), MaxAsked)
	}
	return 0
}

// pushFamily is the push service a device push address belongs to: the last two parts of its host
// name, for example "googleapis.com" (Chrome, Android), "apple.com", "mozilla.com" or "windows.com".
// Each push service checks Seyal's key on its own, so a note one of them took says nothing about the
// others (the key guard, endRefused). It is never logged.
func pushFamily(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.ToLower(u.Hostname()), ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}

// nextTry is when a phone note no device took is tried again, after its tries-th try: the usual wait,
// or longer if a push service asked. ok is false when it is given up.
func nextTry(tries int, asked time.Duration, now time.Time) (at time.Time, ok bool) {
	if tries < 1 || tries >= MaxTries {
		return time.Time{}, false
	}
	return now.Add(max(RetryWaits[tries-1], asked)), true
}

// payload is the phone note as the service worker reads it (web/templates/sw.js), with the link built
// from the address the device used when it turned phone notes on. "web_push": 8030 is the fixed marker
// Apple's Declarative Web Push asks for.
func (n Note) payload(origin string) ([]byte, error) {
	type notification struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		Navigate string `json:"navigate"`
		Tag      string `json:"tag"`
	}
	return json.Marshal(struct {
		WebPush      int          `json:"web_push"`
		Notification notification `json:"notification"`
	}{8030, notification{Title: Title, Body: n.Body, Navigate: origin + n.Path, Tag: n.Tag}})
}

// round is what one business's Deliver saw from the push services: how many devices and people were
// posted to, and which devices said 401 or 403. Those end at the end of the pass, if the guard allows
// (endRefused).
type round struct {
	tried   int // devices
	people  int // people (each gets at most one phone note in a pass)
	refused []auth.BusinessDevice
}
