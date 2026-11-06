// SPDX-License-Identifier: AGPL-3.0-or-later

// Package push sends phone notes through the browsers' push services (web push). It locks each
// note so only the one browser can read it (RFC 8291), signs Seyal's "ID card" for the push service
// (VAPID, RFC 8292), and posts only to the push services' own hosts, through the safe outgoing
// caller in internal/send. Everything here is the standard library; no push service account is
// needed.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const (
	// recordSize is the size the header announces. A phone note is always one record, so any size
	// above the note's length works; 4096 is what every push library uses.
	recordSize = 4096
	// headerSize is salt (16) + record size (4) + key length (1) + Seyal's one-time public key (65).
	headerSize = 16 + 4 + 1 + 65
	// MaxBody is the longest locked note Seyal sends. Firefox on Android takes at most 2,744 bytes,
	// the smallest limit of the four push services.
	MaxBody = 2744
	// MaxPayload is the longest note before locking that still fits in MaxBody (one byte marks the
	// end of the record, and 16 bytes are the lock's check code).
	MaxPayload = MaxBody - headerSize - 1 - 16
)

// ErrTooLong is returned for a note longer than MaxPayload.
var ErrTooLong = errors.New("push: the phone note is too long")

// Lock locks plain for one browser, the way RFC 8291 says ("aes128gcm" content coding). p256dh is
// the browser's public key (65 bytes) and auth its 16-byte secret, both given when it turned phone
// notes on. Each call uses a new one-time key and salt, so locking the same note twice gives
// different bytes.
func Lock(plain, p256dh, auth []byte) ([]byte, error) {
	oneTime, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return lock(plain, p256dh, auth, oneTime, salt)
}

// lock is Lock with the one-time key and the salt given, so the RFC's worked example can be checked.
func lock(plain, p256dh, auth []byte, oneTime *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plain) > MaxPayload {
		return nil, ErrTooLong
	}
	if len(auth) != 16 || len(salt) != 16 {
		return nil, errors.New("push: the device's secret must be 16 bytes")
	}
	browser, err := ecdh.P256().NewPublicKey(p256dh)
	if err != nil {
		return nil, errors.New("push: the device's public key is not a P-256 key")
	}
	shared, err := oneTime.ECDH(browser)
	if err != nil {
		return nil, err
	}
	ourPublic := oneTime.PublicKey().Bytes()

	// The keys, in the RFC's words: IKM from the shared secret and the browser's secret; then the
	// content key (CEK) and the nonce from IKM and the salt. hkdf.Key is "extract, then expand".
	keyInfo := "WebPush: info\x00" + string(p256dh) + string(ourPublic)
	ikm, err := hkdf.Key(sha256.New, shared, auth, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// The header: salt, record size, then our one-time public key with its length in front.
	out := make([]byte, 0, headerSize+len(plain)+1+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(ourPublic)))
	out = append(out, ourPublic...)

	// One record: the note, then 0x02, which means "this is the last record" (no padding after it).
	record := append(append(make([]byte, 0, len(plain)+1), plain...), 0x02)
	return gcm.Seal(out, nonce, record, nil), nil
}
