// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signing follows "Standard Webhooks" v1.0.0
// (https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md):
//
//	webhook-id:        the send id, the same on every try
//	webhook-timestamp: seconds since 1970, as text
//	webhook-signature: "v1," + base64( HMAC-SHA256( secret, id + "." + timestamp + "." + body ) )
//
// The secret is written "whsec_" + base64 of its bytes; the HMAC key is the bytes, not the text.

// secretPrefix starts every signing secret, so a person can tell it from other keys.
const secretPrefix = "whsec_"

// secretBytes is how many random bytes a new secret has (the spec asks for 24 to 64).
const secretBytes = 32

// NewSecret makes a new random signing secret, "whsec_" + base64.
func NewSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("send: could not make a signing secret")
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// secretKey reads a signing secret back into the bytes the HMAC uses. Its errors never hold the
// secret.
func secretKey(secret string) ([]byte, error) {
	b64, ok := strings.CutPrefix(strings.TrimSpace(secret), secretPrefix)
	if !ok {
		return nil, errors.New("send: a signing secret must start with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, errors.New("send: a signing secret must be 24 to 64 bytes in base64")
	}
	return key, nil
}

// checkID refuses a send id that can't go in a header or would make the signed text unclear.
func checkID(id string) error {
	if id == "" || len(id) > 200 || strings.ContainsAny(id, ". \t\r\n") {
		return errors.New("send: a send id must be 1 to 200 characters, with no dot or space")
	}
	return nil
}

// Sign returns the "webhook-signature" value for body: "v1," and the base64 HMAC.
func Sign(secret, id string, at time.Time, body []byte) (string, error) {
	key, err := secretKey(secret)
	if err != nil {
		return "", err
	}
	if err := checkID(id); err != nil {
		return "", err
	}
	return sign(key, id, strconv.FormatInt(at.Unix(), 10), body), nil
}

func sign(key []byte, id, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + timestamp + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Signed gets one send ready: the headers (webhook-id, webhook-timestamp, webhook-signature and
// Content-Type) and the exact bytes to post. body must be a JSON object.
//
// With inBody (for a target that can't read headers, see SigInBody) the bytes also carry the
// signature at the very end, as one more member:
//
//	{...the body...,"sig":{"id":"snd_…","timestamp":"1614265330","signature":"v1,…"}}
//
// That "sig" signature is over the body *without* it. The other side must check it in exactly
// these steps, and refuse the post if any step fails:
//
//  1. Find the last `,"sig":` (for an empty body, the post starts with `{"sig":`). The text before
//     it, with "}" put back, is the signed text.
//  2. After `,"sig":` must come one JSON object and then a single "}", with nothing after it, not
//     even a space. Anything more (say `,"data":{...}` added after "sig") is refused: JSON readers
//     keep the last copy of a repeated name, so added text could swap the data.
//  3. Check the HMAC of id + "." + timestamp + "." + the signed text, and the timestamp's age.
//  4. Read the data from the signed text only, never from the whole post.
//
// The header signature is always over the exact bytes posted, as the spec says, so it differs from
// the "sig" one when inBody is set.
func Signed(secret, id string, at time.Time, body []byte, inBody bool) (http.Header, []byte, error) {
	key, err := secretKey(secret)
	if err != nil {
		return nil, nil, err
	}
	if err := checkID(id); err != nil {
		return nil, nil, err
	}
	body = []byte(strings.TrimSpace(string(body)))
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || len(body) < 2 || body[0] != '{' {
		return nil, nil, errors.New("send: the body must be a JSON object")
	}
	if _, taken := obj["sig"]; taken && inBody {
		return nil, nil, errors.New("send: the body already has a sig member")
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	out := body
	if inBody {
		sig, err := json.Marshal(struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			Signature string `json:"signature"`
		}{id, ts, sign(key, id, ts, body)})
		if err != nil {
			return nil, nil, err
		}
		comma := ","
		if len(obj) == 0 {
			comma = ""
		}
		out = make([]byte, 0, len(body)+len(sig)+16)
		out = append(out, body[:len(body)-1]...) // the body without its closing "}"
		out = append(out, comma+`"sig":`...)
		out = append(out, sig...)
		out = append(out, '}')
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", sign(key, id, ts, out))
	return h, out, nil
}

// scriptHost is the host of Google's Apps Script web apps. Apps Script can't read a request's
// headers, so a target there gets the signature inside the body as well. A fixed value, like the
// push hosts: it is a host name, not a business word.
const scriptHost = "script.google.com"

// SigInBody reports whether a target on host (lower case, no port) needs the signature inside the
// body too.
func SigInBody(host string) bool { return host == scriptHost }
