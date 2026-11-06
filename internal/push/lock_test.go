// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
)

// The worked example of RFC 8291, Appendix A (and section 5): fixed keys, a fixed salt, and the
// exact bytes that must come out.
const (
	rfcPlain      = "When I grow up, I want to be a watermelon"
	rfcASPrivate  = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcASPublic   = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	rfcUAPrivate  = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcUAPublic   = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcSalt       = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcAuthSecret = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcBody       = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decode(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestLockRFC8291Example(t *testing.T) {
	// The one-time key is fixed for the test. (Since Go 1.26, ecdh's GenerateKey ignores the random
	// source it is given, so a fixed key is made from its bytes instead.)
	oneTime, err := ecdh.P256().NewPrivateKey(mustDecode(t, rfcASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	if got := b64.EncodeToString(oneTime.PublicKey().Bytes()); got != rfcASPublic {
		t.Fatalf("the example's server public key: got %s", got)
	}
	got, err := lock([]byte(rfcPlain), mustDecode(t, rfcUAPublic), mustDecode(t, rfcAuthSecret), oneTime, mustDecode(t, rfcSalt))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustDecode(t, rfcBody); !bytes.Equal(got, want) {
		t.Fatalf("locked bytes differ from RFC 8291 Appendix A\n got %s\nwant %s", b64.EncodeToString(got), rfcBody)
	}

	// And the browser's side: its private key opens the RFC's bytes again.
	ua, err := ecdh.P256().NewPrivateKey(mustDecode(t, rfcUAPrivate))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := unlock(mustDecode(t, rfcBody), ua, mustDecode(t, rfcAuthSecret))
	if err != nil || string(plain) != rfcPlain {
		t.Fatalf("unlock = %q, %v", plain, err)
	}
}

func TestLockRoundTripAndLimits(t *testing.T) {
	ua, err := ecdh.P256().NewPrivateKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	auth := bytes.Repeat([]byte{1}, 16)
	note := []byte(`{"web_push":8030,"notification":{"title":"Seyal","body":"New work from Ben: J-2026-0007"}}`)

	a, err := Lock(note, ua.PublicKey().Bytes(), auth)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Lock(note, ua.PublicKey().Bytes(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("two locks of the same note gave the same bytes; the one-time key and salt must be new each time")
	}
	if bytes.Contains(a, []byte("J-2026-0007")) {
		t.Error("the locked bytes still show the note's text")
	}
	for _, body := range [][]byte{a, b} {
		if got, err := unlock(body, ua, auth); err != nil || !bytes.Equal(got, note) {
			t.Errorf("unlock = %q, %v", got, err)
		}
	}

	// The longest note fits Firefox on Android's limit exactly; one byte more is refused.
	longest, err := Lock(bytes.Repeat([]byte("a"), MaxPayload), ua.PublicKey().Bytes(), auth)
	if err != nil || len(longest) != MaxBody {
		t.Errorf("longest note: %d bytes, %v; want %d", len(longest), err, MaxBody)
	}
	if _, err := Lock(bytes.Repeat([]byte("a"), MaxPayload+1), ua.PublicKey().Bytes(), auth); !errors.Is(err, ErrTooLong) {
		t.Errorf("a note one byte too long: %v, want ErrTooLong", err)
	}

	// Bad keys are refused.
	if _, err := Lock(note, []byte("not a key"), auth); err == nil {
		t.Error("a bad public key was accepted")
	}
	if _, err := Lock(note, ua.PublicKey().Bytes(), auth[:15]); err == nil {
		t.Error("a 15-byte secret was accepted")
	}
}

// unlock is the browser's side of RFC 8291, for the tests only: it reads the header, makes the same
// keys from the browser's private key, and opens the one record.
func unlock(body []byte, ua *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("too short")
	}
	salt, idLen := body[:16], int(body[20])
	if binary.BigEndian.Uint32(body[16:20]) != recordSize || len(body) < 21+idLen {
		return nil, errors.New("bad header")
	}
	serverPublic := body[21 : 21+idLen]
	sp, err := ecdh.P256().NewPublicKey(serverPublic)
	if err != nil {
		return nil, err
	}
	shared, err := ua.ECDH(sp)
	if err != nil {
		return nil, err
	}
	ikm, _ := hkdf.Key(sha256.New, shared, auth, "WebPush: info\x00"+string(ua.PublicKey().Bytes())+string(serverPublic), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	record, err := gcm.Open(nil, nonce, body[21+idLen:], nil)
	if err != nil {
		return nil, err
	}
	record = bytes.TrimRight(record, "\x00") // padding, if any
	if len(record) == 0 || record[len(record)-1] != 0x02 {
		return nil, errors.New("no last-record mark")
	}
	return record[:len(record)-1], nil
}
