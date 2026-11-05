// SPDX-License-Identifier: AGPL-3.0-or-later

package seal

import (
	"errors"
	"strings"
	"testing"
)

type note struct {
	Who   string `json:"who"`
	Words string `json:"words"`
}

func TestSignAndOpen(t *testing.T) {
	key := NewKey()
	token, err := Sign(key, note{Who: "p1", Words: "Acme Traders called, அனுப்புங்க"})
	if err != nil {
		t.Fatal(err)
	}
	var got note
	if err := Open(key, token, &got); err != nil || got.Who != "p1" || got.Words != "Acme Traders called, அனுப்புங்க" {
		t.Fatalf("Open = %+v, %v", got, err)
	}
	if strings.ContainsAny(token, "+/= ") {
		t.Errorf("token %q has characters that don't fit a form field or URL", token)
	}
}

func TestOpenRefuses(t *testing.T) {
	key := NewKey()
	token, _ := Sign(key, note{Who: "p1", Words: "pay 500"})
	other, _ := Sign(NewKey(), note{Who: "p1", Words: "pay 500"})
	notePart, sig, _ := strings.Cut(token, ".")
	changed, _ := Sign(key, note{Who: "p1", Words: "pay 900"})
	changedNote, _, _ := strings.Cut(changed, ".")

	for name, tok := range map[string]string{
		"another key":          other,
		"changed note":         changedNote + "." + sig,
		"no signature":         notePart,
		"empty":                "",
		"signature cut":        notePart + "." + sig[:len(sig)-2],
		"not base64":           "!!!." + sig,
		"note and sig swapped": sig + "." + notePart,
	} {
		var got note
		if err := Open(key, tok, &got); !errors.Is(err, ErrBroken) {
			t.Errorf("%s: Open = %v, want ErrBroken", name, err)
		}
	}
}
