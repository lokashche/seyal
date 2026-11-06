// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"context"
	"crypto/ecdh"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"seyal/internal/send"
)

// Device is one browser that turned phone notes on: its device push address (endpoint) and its two
// lock keys, all as the browser gave them (p256dh and auth in base64url).
type Device struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Check checks a device before it is saved or used: the address is on a push service, p256dh is a
// real P-256 public key and auth is 16 bytes. Its errors never hold the address or a key.
func (d Device) Check() error {
	_, _, err := d.keys()
	return err
}

func (d Device) keys() (p256dh, auth []byte, err error) {
	if err := CheckEndpoint(d.Endpoint); err != nil {
		return nil, nil, errors.New("push: the device push address is not on a push service (" + err.Error() + ")")
	}
	p256dh, err = decode(d.P256dh)
	if err != nil || len(p256dh) != 65 {
		return nil, nil, errors.New("push: the device's public key is not a P-256 key")
	}
	if _, err := ecdh.P256().NewPublicKey(p256dh); err != nil {
		return nil, nil, errors.New("push: the device's public key is not a P-256 key")
	}
	auth, err = decode(d.Auth)
	if err != nil || len(auth) != 16 {
		return nil, nil, errors.New("push: the device's secret must be 16 bytes")
	}
	return p256dh, auth, nil
}

// Message is one phone note on its way, with the labels the push service reads (RFC 8030).
type Message struct {
	Payload []byte        // the note itself (JSON for the service worker); it is locked before it leaves
	TTL     time.Duration // how long the push service may keep it for a phone that is offline
	Urgency string        // "very-low", "low", "normal" or "high"; empty means "normal"
	Topic   string        // a short code (at most 32 letters, digits, - or _), so a newer note replaces an older one; empty for none
}

var urgencies = map[string]bool{"very-low": true, "low": true, "normal": true, "high": true}

func (m Message) check() error {
	if m.TTL < 0 {
		return errors.New("push: TTL can't be negative")
	}
	if m.Urgency != "" && !urgencies[m.Urgency] {
		return errors.New("push: Urgency must be very-low, low, normal or high")
	}
	if len(m.Topic) > 32 {
		return errors.New("push: Topic is longer than 32 characters")
	}
	for _, r := range m.Topic {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return errors.New("push: Topic may hold only letters, digits, - and _")
		}
	}
	return nil
}

// The safe outgoing caller's limits for push: 15 seconds, and at most 4 KB of a reply read.
const (
	callTimeout = 15 * time.Second
	maxReply    = 4 << 10
	// tokenLife is how long each VAPID token is valid; RFC 8292 allows up to 24 hours.
	tokenLife = 12 * time.Hour
)

// Sender locks and posts phone notes. Make one with NewSender; it is safe to share.
type Sender struct {
	keys    *Keys
	contact string
	now     func() time.Time
	// post is the outgoing call: the safe caller in internal/send, allowed only the push hosts. A
	// test swaps it for a fake, so no test ever calls a real push service.
	post func(ctx context.Context, address string, header http.Header, body []byte) (send.Answer, error)
}

// NewSender returns a sender with the server's keys and contact (SEYAL_PUSH_CONTACT: "mailto:..."
// or "https://...").
func NewSender(keys *Keys, contact string) (*Sender, error) {
	if keys == nil {
		return nil, errors.New("push: no keys")
	}
	if _, err := keys.Token("https://"+fcmHost, contact, time.Now(), time.Now().Add(time.Hour)); err != nil {
		return nil, err
	}
	caller := send.New(IsPushHost, callTimeout, maxReply)
	return &Sender{keys: keys, contact: contact, now: time.Now, post: caller.Post}, nil
}

// Send locks m for d and posts it to d's push service. It returns the push service's answer (201
// means "accepted", not "seen"); what each code means for the device is the caller's choice. Its
// errors never hold the device push address: a failed call is a *send.Failure with a fixed reason.
func (s *Sender) Send(ctx context.Context, d Device, m Message) (send.Answer, error) {
	p256dh, auth, err := d.keys()
	if err != nil {
		return send.Answer{}, err
	}
	if err := m.check(); err != nil {
		return send.Answer{}, err
	}
	body, err := Lock(m.Payload, p256dh, auth)
	if err != nil {
		return send.Answer{}, err
	}
	u, err := url.Parse(d.Endpoint) // already checked by d.keys
	if err != nil {
		return send.Answer{}, errors.New("push: bad device push address")
	}
	now := s.now()
	token, err := s.keys.Token("https://"+strings.ToLower(u.Hostname()), s.contact, now, now.Add(tokenLife))
	if err != nil {
		return send.Answer{}, err
	}
	h := http.Header{}
	h.Set("Authorization", s.keys.Authorization(token))
	h.Set("Content-Encoding", "aes128gcm")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("TTL", strconv.FormatInt(int64(m.TTL/time.Second), 10))
	if m.Urgency != "" {
		h.Set("Urgency", m.Urgency)
	}
	if m.Topic != "" {
		h.Set("Topic", m.Topic)
	}
	return s.post(ctx, d.Endpoint, h, body)
}
