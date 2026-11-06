// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"seyal/internal/send"
)

// fakeDevice is a browser for the tests: its private key stays here, so the test can open what
// Seyal locked for it.
func fakeDevice(t *testing.T, endpoint string) (Device, *ecdh.PrivateKey, []byte) {
	t.Helper()
	ua, err := ecdh.P256().NewPrivateKey(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	auth := bytes.Repeat([]byte{3}, 16)
	return Device{Endpoint: endpoint, P256dh: b64.EncodeToString(ua.PublicKey().Bytes()), Auth: b64.EncodeToString(auth)}, ua, auth
}

func TestSend(t *testing.T) {
	keys, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSender(keys, "mailto:admin@kaveri.example")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 11, 10, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	// The fake push service: it records what Seyal would post. Nothing leaves the test.
	var gotAddress string
	var gotHeader http.Header
	var gotBody []byte
	s.post = func(_ context.Context, address string, h http.Header, body []byte) (send.Answer, error) {
		gotAddress, gotHeader, gotBody = address, h, body
		return send.Answer{Status: http.StatusCreated}, nil
	}

	const endpoint = "https://web.push.apple.com/QFake0000"
	dev, ua, auth := fakeDevice(t, endpoint)
	note := []byte(`{"web_push":8030,"notification":{"title":"Seyal","body":"New work from Ben: J-2026-0007"}}`)
	ans, err := s.Send(context.Background(), dev, Message{Payload: note, TTL: 12 * time.Hour, Urgency: "normal", Topic: "n7f3a9c2"})
	if err != nil || ans.Status != http.StatusCreated {
		t.Fatalf("Send = %+v, %v", ans, err)
	}
	if gotAddress != endpoint {
		t.Errorf("posted to %q, want %q", gotAddress, endpoint)
	}
	for k, want := range map[string]string{
		"Content-Encoding": "aes128gcm",
		"Content-Type":     "application/octet-stream",
		"TTL":              "43200",
		"Urgency":          "normal",
		"Topic":            "n7f3a9c2",
	} {
		if got := gotHeader.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	// The token is for Apple's origin, signed with the server's key, and valid for 12 hours.
	authz := gotHeader.Get("Authorization")
	tok, pub, ok := strings.Cut(strings.TrimPrefix(authz, "vapid t="), ", k=")
	if !strings.HasPrefix(authz, "vapid t=") || !ok || pub != keys.Public() {
		t.Fatalf("Authorization = %q", authz)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(mustDecode(t, strings.Split(tok, ".")[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://web.push.apple.com" || claims.Exp != now.Add(12*time.Hour).Unix() {
		t.Errorf("claims = %+v", claims)
	}

	// Only the phone can read the note: the body is locked, and the phone's key opens it.
	if bytes.Contains(gotBody, []byte("J-2026-0007")) {
		t.Error("the posted body shows the note's text")
	}
	if plain, err := unlock(gotBody, ua, auth); err != nil || !bytes.Equal(plain, note) {
		t.Errorf("the phone opens %q, %v", plain, err)
	}

	// No Urgency or Topic: the headers are left out.
	if _, err := s.Send(context.Background(), dev, Message{Payload: note, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if gotHeader.Get("Urgency") != "" || gotHeader.Get("Topic") != "" || gotHeader.Get("TTL") != "60" {
		t.Errorf("headers = %v", gotHeader)
	}
}

func TestSendRefuses(t *testing.T) {
	keys, _ := GenerateKeys()
	s, err := NewSender(keys, "mailto:admin@kaveri.example")
	if err != nil {
		t.Fatal(err)
	}
	posted := 0
	s.post = func(context.Context, string, http.Header, []byte) (send.Answer, error) {
		posted++
		return send.Answer{Status: http.StatusCreated}, nil
	}
	note := []byte(`{"web_push":8030}`)
	good, _, _ := fakeDevice(t, "https://fcm.googleapis.com/wp/fake0000")

	// A device push address off the list: refused before anything is locked or posted, and the
	// error doesn't show the address (it holds a marker word to look for).
	const marker = "marker7Q2Z"
	bad := good
	bad.Endpoint = "https://evil.example/" + marker
	if _, err := s.Send(context.Background(), bad, Message{Payload: note}); err == nil || strings.Contains(err.Error(), marker) {
		t.Errorf("an address off the list: err = %v", err)
	}

	noKey := good
	noKey.P256dh = b64.EncodeToString(bytes.Repeat([]byte{4}, 65)) // 65 bytes, but not a point on the curve
	shortAuth := good
	shortAuth.Auth = b64.EncodeToString([]byte("short"))
	for name, d := range map[string]Device{"not a P-256 key": noKey, "auth not 16 bytes": shortAuth} {
		if err := d.Check(); err == nil {
			t.Errorf("%s: Check accepted it", name)
		}
		if _, err := s.Send(context.Background(), d, Message{Payload: note}); err == nil {
			t.Errorf("%s: Send accepted it", name)
		}
	}
	if err := good.Check(); err != nil {
		t.Errorf("a good device: %v", err)
	}

	for name, m := range map[string]Message{
		"unknown urgency":  {Payload: note, Urgency: "urgent"},
		"topic too long":   {Payload: note, Topic: strings.Repeat("a", 33)},
		"topic with a dot": {Payload: note, Topic: "J-2026.0007"},
		"negative TTL":     {Payload: note, TTL: -time.Second},
		"note too long":    {Payload: bytes.Repeat([]byte("a"), MaxPayload+1)},
	} {
		if _, err := s.Send(context.Background(), good, m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if posted != 0 {
		t.Errorf("%d posts for refused sends, want 0", posted)
	}

	if _, err := NewSender(keys, "admin@kaveri.example"); err == nil {
		t.Error("a contact without mailto: was accepted")
	}
	if _, err := NewSender(nil, "mailto:admin@kaveri.example"); err == nil {
		t.Error("no keys was accepted")
	}
}

// The real sender goes through the safe outgoing caller, which allows only the push hosts.
func TestSenderUsesTheSafeCaller(t *testing.T) {
	keys, _ := GenerateKeys()
	s, err := NewSender(keys, "mailto:admin@kaveri.example")
	if err != nil {
		t.Fatal(err)
	}
	// Even past the device check, the caller itself refuses any host that isn't a push service
	// (here this computer). It refuses before connecting, so no network is used.
	for _, address := range []string{"https://127.0.0.1/wp/fake0000", "https://evil.example/wp/fake0000"} {
		_, err = s.post(context.Background(), address, http.Header{}, nil)
		if f, ok := err.(*send.Failure); !ok || f.Reason != send.ReasonHost {
			t.Errorf("the caller allowed %s: %v", address, err)
		}
	}
}
