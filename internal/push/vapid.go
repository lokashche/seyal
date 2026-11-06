// SPDX-License-Identifier: AGPL-3.0-or-later

package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Keys are the server's VAPID key pair (RFC 8292): the "ID card" a push service checks. They need
// no account anywhere. The public key also goes to each browser when it turns phone notes on, so
// changing the keys ends every device's phone notes.
type Keys struct {
	private *ecdsa.PrivateKey
	public  string // base64url, the 65-byte uncompressed point the browsers expect
}

// b64 is the encoding web push uses everywhere: base64url without "=" padding.
var b64 = base64.RawURLEncoding

// decode reads base64url with or without "=" padding.
func decode(s string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(s, "="))
}

// GenerateKeys makes a new key pair. Nothing leaves the computer.
func GenerateKeys() (*Keys, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return newKeys(priv)
}

func newKeys(priv *ecdsa.PrivateKey) (*Keys, error) {
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &Keys{private: priv, public: b64.EncodeToString(pub)}, nil
}

// ParseKeys reads the two keys as `seyal push keys` printed them, and checks that the public key
// belongs to the private key. Its errors never hold a key.
func ParseKeys(public, private string) (*Keys, error) {
	raw, err := decode(strings.TrimSpace(private))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("push: SEYAL_PUSH_PRIVATE_KEY is not a key printed by `seyal push keys`")
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, errors.New("push: SEYAL_PUSH_PRIVATE_KEY is not a valid P-256 key")
	}
	k, err := newKeys(priv)
	if err != nil {
		return nil, err
	}
	if pub, err := decode(strings.TrimSpace(public)); err != nil || b64.EncodeToString(pub) != k.public {
		return nil, errors.New("push: SEYAL_PUSH_PUBLIC_KEY doesn't belong to SEYAL_PUSH_PRIVATE_KEY")
	}
	return k, nil
}

// Public is the public key, for the browser and for the push service.
func (k *Keys) Public() string { return k.public }

// Private is the private key, written out once by `seyal push keys`. Keep it secret.
func (k *Keys) Private() string {
	raw, err := k.private.Bytes()
	if err != nil {
		return "" // only for a broken key, which newKeys never makes
	}
	return b64.EncodeToString(raw)
}

// MaxTokenLife is the longest a token may be valid: RFC 8292 allows at most 24 hours.
const MaxTokenLife = 24 * time.Hour

// Token makes the signed VAPID token (a JWT signed with ES256) for one push service. audience is
// the push service's origin ("https://fcm.googleapis.com"), contact is "mailto:..." or "https://..."
// (the push service writes there if something is wrong), and the token stops working at expires.
func (k *Keys) Token(audience, contact string, now, expires time.Time) (string, error) {
	if !strings.HasPrefix(audience, "https://") || strings.Count(audience, "/") != 2 {
		return "", errors.New("push: the audience must be an origin like https://host")
	}
	if !strings.HasPrefix(contact, "mailto:") && !strings.HasPrefix(contact, "https://") {
		return "", errors.New("push: SEYAL_PUSH_CONTACT must start with mailto: or https://")
	}
	if !expires.After(now) || expires.Sub(now) > MaxTokenLife {
		return "", errors.New("push: a token must expire within 24 hours")
	}
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{audience, expires.Unix(), contact})
	if err != nil {
		return "", err
	}
	unsigned := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, sum[:])
	if err != nil {
		return "", err
	}
	// ES256 writes the two halves of the signature side by side, each exactly 32 bytes.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return unsigned + "." + b64.EncodeToString(sig), nil
}

// Authorization is the header value a push service expects: the token and the public key.
func (k *Keys) Authorization(token string) string {
	return "vapid t=" + token + ", k=" + k.public
}
