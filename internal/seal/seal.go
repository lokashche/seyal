// SPDX-License-Identifier: AGPL-3.0-or-later

// Package seal signs small notes that the server gives to a browser and later gets back, so the
// server can tell that a note is its own and unchanged. A note is JSON, signed with HMAC-SHA256 (a
// fingerprint that only someone holding the key can make). The note itself is readable by anyone:
// never put secrets in it.
package seal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// ErrBroken means a token wasn't made with this key, or was changed since.
var ErrBroken = errors.New("seal: the token isn't ours or was changed")

// NewKey returns a new random key of 32 bytes.
func NewKey() []byte {
	key := make([]byte, 32)
	rand.Read(key) // never fails (crypto/rand panics if the system can't give random bytes)
	return key
}

var b64 = base64.RawURLEncoding

// Sign turns v into a token: the note as JSON, then a dot, then its signature, both in base64url
// (letters, digits, "-" and "_", so the token fits in a form field or a URL).
func Sign(key []byte, v any) (string, error) {
	note, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(note) + "." + b64.EncodeToString(mac(key, note)), nil
}

// Open checks a token made by Sign with the same key and reads its note into v. It returns
// ErrBroken for any token that wasn't made with this key or was changed in any way.
func Open(key []byte, token string, v any) error {
	notePart, sigPart, ok := strings.Cut(token, ".")
	if !ok {
		return ErrBroken
	}
	note, err1 := b64.DecodeString(notePart)
	sig, err2 := b64.DecodeString(sigPart)
	// hmac.Equal takes the same time however many bytes match, so the time a check takes can't
	// be used to guess a signature byte by byte.
	if err1 != nil || err2 != nil || !hmac.Equal(sig, mac(key, note)) {
		return ErrBroken
	}
	if err := json.Unmarshal(note, v); err != nil {
		return ErrBroken
	}
	return nil
}

func mac(key, note []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(note)
	return h.Sum(nil)
}
