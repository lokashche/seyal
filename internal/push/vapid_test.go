// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestKeysRoundTrip(t *testing.T) {
	k, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	if pub := mustDecode(t, k.Public()); len(pub) != 65 || pub[0] != 4 {
		t.Errorf("public key: %d bytes, first %x; want 65 bytes starting 04 (uncompressed P-256)", len(pub), pub[0])
	}
	if priv := mustDecode(t, k.Private()); len(priv) != 32 {
		t.Errorf("private key: %d bytes, want 32", len(priv))
	}
	back, err := ParseKeys(k.Public(), k.Private())
	if err != nil || back.Public() != k.Public() || back.Private() != k.Private() {
		t.Fatalf("ParseKeys gave %v, %v", back, err)
	}
	// Spaces around a pasted value and "=" padding are fine.
	if _, err := ParseKeys(" "+k.Public()+"= ", k.Private()+"\n"); err != nil {
		t.Errorf("padded or spaced keys refused: %v", err)
	}

	other, _ := GenerateKeys()
	for name, pair := range map[string][2]string{
		"public of another pair": {other.Public(), k.Private()},
		"empty private":          {k.Public(), ""},
		"short private":          {k.Public(), k.Private()[:20]},
		"not base64":             {k.Public(), "%%%%"},
		"empty public":           {"", k.Private()},
	} {
		_, err := ParseKeys(pair[0], pair[1])
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), k.Private()) || strings.Contains(err.Error(), k.Public()) {
			t.Errorf("%s: the error shows a key: %v", name, err)
		}
	}
}

func TestTokenClaims(t *testing.T) {
	k, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 11, 10, 0, 0, time.UTC) // 16:40 in India
	exp := now.Add(12 * time.Hour)
	tok, err := k.Token("https://fcm.googleapis.com", "mailto:admin@kaveri.example", now, exp)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	var header map[string]string
	if err := json.Unmarshal(mustDecode(t, parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header["typ"] != "JWT" || header["alg"] != "ES256" || len(header) != 2 {
		t.Errorf("header = %v", header)
	}

	var claims map[string]any
	if err := json.Unmarshal(mustDecode(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "https://fcm.googleapis.com" || claims["sub"] != "mailto:admin@kaveri.example" ||
		claims["exp"] != float64(exp.Unix()) || len(claims) != 3 {
		t.Errorf("claims = %v", claims)
	}

	// The signature is ES256: 64 bytes (r then s) over "header.claims", checked with the public key.
	sig := mustDecode(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64", len(sig))
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustDecode(t, k.Public()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, sum[:], r, s) {
		t.Error("the signature doesn't check out with the public key")
	}
	other, _ := GenerateKeys()
	otherPub, _ := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustDecode(t, other.Public()))
	if ecdsa.Verify(otherPub, sum[:], r, s) {
		t.Error("the signature checks out with another key too")
	}

	if got, want := k.Authorization(tok), "vapid t="+tok+", k="+k.Public(); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}

	// What the token refuses.
	for name, tc := range map[string]struct {
		aud, sub string
		exp      time.Time
	}{
		"longer than 24 hours":  {"https://fcm.googleapis.com", "mailto:admin@kaveri.example", now.Add(25 * time.Hour)},
		"already expired":       {"https://fcm.googleapis.com", "mailto:admin@kaveri.example", now.Add(-time.Minute)},
		"contact not mailto":    {"https://fcm.googleapis.com", "admin@kaveri.example", exp},
		"audience with a path":  {"https://fcm.googleapis.com/fcm/send", "mailto:admin@kaveri.example", exp},
		"audience not https":    {"http://fcm.googleapis.com", "mailto:admin@kaveri.example", exp},
		"https contact is fine": {"https://fcm.googleapis.com", "https://kaveri.example/contact", exp},
	} {
		_, err := k.Token(tc.aud, tc.sub, now, tc.exp)
		if wantOK := name == "https contact is fine"; (err == nil) != wantOK {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
