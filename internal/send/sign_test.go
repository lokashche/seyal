// SPDX-License-Identifier: AGPL-3.0-or-later

package send

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The worked example in the Standard Webhooks spec v1.0.0 (its "Signature scheme" section).
const (
	specSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	specID        = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	specBody      = `{"test": 2432232314}`
	specSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

var specTime = time.Unix(1614265330, 0)

func TestSignMatchesTheSpecExample(t *testing.T) {
	got, err := Sign(specSecret, specID, specTime, []byte(specBody))
	if err != nil {
		t.Fatal(err)
	}
	if got != specSignature {
		t.Errorf("Sign = %s, want the spec's %s", got, specSignature)
	}

	h, out, err := Signed(specSecret, specID, specTime, []byte(specBody), false)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != specBody {
		t.Errorf("the body changed: %s", out)
	}
	for k, want := range map[string]string{
		"webhook-id": specID, "webhook-timestamp": "1614265330", "webhook-signature": specSignature,
		"Content-Type": "application/json",
	} {
		if h.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, h.Get(k), want)
		}
	}
}

// checkInBody is what the sheet's script will do (part 4c-4), written here in Go so the rule in
// Signed's comment is checked now. It returns the signed text, the only text the data may be read
// from.
func checkInBody(secret string, post []byte, now time.Time) ([]byte, error) {
	key, err := secretKey(secret)
	if err != nil {
		return nil, err
	}
	var signed, rest []byte
	if i := bytes.LastIndex(post, []byte(`,"sig":`)); i >= 0 {
		signed = append(append([]byte{}, post[:i]...), '}')
		rest = post[i+len(`,"sig":`):]
	} else if bytes.HasPrefix(post, []byte(`{"sig":`)) {
		signed = []byte(`{}`)
		rest = post[len(`{"sig":`):]
	} else {
		return nil, errors.New("no sig")
	}
	dec := json.NewDecoder(bytes.NewReader(rest))
	dec.DisallowUnknownFields()
	var sig struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		Signature string `json:"signature"`
	}
	if err := dec.Decode(&sig); err != nil {
		return nil, errors.New("sig is not one object")
	}
	if string(rest[dec.InputOffset():]) != "}" {
		return nil, errors.New("something after sig")
	}
	if !hmac.Equal([]byte(sign(key, sig.ID, sig.Timestamp, signed)), []byte(sig.Signature)) {
		return nil, errors.New("bad signature")
	}
	ts, err := strconv.ParseInt(sig.Timestamp, 10, 64)
	if err != nil || now.Sub(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return nil, errors.New("too old")
	}
	return signed, nil
}

func TestSignedInBody(t *testing.T) {
	h, out, err := Signed(specSecret, specID, specTime, []byte(specBody), true)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Test int64 `json:"test"`
		Sig  struct{ ID, Timestamp, Signature string }
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("the body with sig is not JSON: %v\n%s", err, out)
	}
	if parsed.Test != 2432232314 || parsed.Sig.ID != specID || parsed.Sig.Timestamp != "1614265330" {
		t.Errorf("parsed = %+v", parsed)
	}
	if parsed.Sig.Signature != specSignature {
		t.Errorf("the sig inside = %s, want the spec's %s (over the body without it)", parsed.Sig.Signature, specSignature)
	}
	signed, err := checkInBody(specSecret, out, specTime)
	if err != nil {
		t.Fatalf("the checker refuses Seyal's own post: %v\n%s", err, out)
	}
	if string(signed) != specBody {
		t.Errorf("the signed text is %s, want %s", signed, specBody)
	}
	// The header signature is over the exact bytes posted, as the spec says.
	if want, _ := Sign(specSecret, specID, specTime, out); h.Get("webhook-signature") != want {
		t.Error("the header signature is not over the posted bytes")
	}

	// Someone who saw one signed post can't add to it or change it.
	cut := string(out[:len(out)-1]) // the post without its last "}"
	for name, bad := range map[string]string{
		"a member added after sig":  cut + `,"data":{"row":["=HYPERLINK(1)"]}}`,
		"a repeated name after sig": cut + `,"test":1}`,
		"a space after sig":         cut + ` }`,
		"text after the end":        string(out) + `x`,
		"a second sig":              cut + `,"sig":` + string(out[bytes.LastIndex(out, []byte(`,"sig":`))+7:]),
		"the body changed":          strings.Replace(string(out), "2432232314", "2432232315", 1),
		"no sig":                    specBody,
	} {
		if _, err := checkInBody(specSecret, []byte(bad), specTime); err == nil {
			t.Errorf("%s: the checker took %s", name, bad)
		}
	}
	if _, err := checkInBody(specSecret, out, specTime.Add(6*time.Minute)); err == nil {
		t.Error("the checker took a post 6 minutes old")
	}

	// An empty object gets no comma, and still checks.
	_, out, err = Signed(specSecret, specID, specTime, []byte(`{}`), true)
	if err != nil || !strings.HasPrefix(string(out), `{"sig":{`) || !json.Valid(out) {
		t.Errorf("empty body with sig = %s, %v", out, err)
	}
	if signed, err := checkInBody(specSecret, out, specTime); err != nil || string(signed) != `{}` {
		t.Errorf("empty body: checker gives %s, %v", signed, err)
	}
	if _, err := checkInBody(specSecret, append(out[:len(out)-1:len(out)-1], `,"data":1}`...), specTime); err == nil {
		t.Error("empty body: the checker took a member added after sig")
	}
}

func TestSignedRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		secret, id, body string
		inBody           bool
	}{
		"secret without whsec_": {"MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", specID, specBody, false},
		"secret too short":      {"whsec_c2hvcnQ=", specID, specBody, false},
		"secret not base64":     {"whsec_!!!!", specID, specBody, false},
		"empty id":              {specSecret, "", specBody, false},
		"id with a dot":         {specSecret, "snd.1", specBody, false},
		"id with a line break":  {specSecret, "snd\r\n1", specBody, false},
		"body not an object":    {specSecret, specID, `[1,2]`, false},
		"body not JSON":         {specSecret, specID, `{"a":`, false},
		"body has sig already":  {specSecret, specID, `{"sig":1}`, true},
	} {
		_, _, err := Signed(c.secret, c.id, specTime, []byte(c.body), c.inBody)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw") {
			t.Errorf("%s: the error shows the secret: %v", name, err)
		}
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if a == b || !strings.HasPrefix(a, "whsec_") {
		t.Errorf("secrets %q and %q", a, b)
	}
	if key, err := secretKey(a); err != nil || len(key) != secretBytes {
		t.Errorf("a new secret reads back as %d bytes, %v", len(key), err)
	}
}

func TestSigInBody(t *testing.T) {
	if !SigInBody("script.google.com") {
		t.Error("an Apps Script target gets no sig in the body")
	}
	for _, h := range []string{"hook.eu1.make.com", "hooks.zapier.com", "n8n.kaveri-demo.example", "script.googleusercontent.com"} {
		if SigInBody(h) {
			t.Errorf("%s gets a sig in the body; only Apps Script should", h)
		}
	}
}
