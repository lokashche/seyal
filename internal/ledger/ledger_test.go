// SPDX-License-Identifier: AGPL-3.0-or-later

package ledger

import (
	"bytes"
	"testing"
)

func TestValidate(t *testing.T) {
	const person = "00000000-0000-0000-0000-000000000001"
	ok := Event{Kind: "item.started", SubjectType: "work_item", ActorKind: Human, ActorPersonID: person}

	tests := []struct {
		name    string
		change  func(e *Event)
		wantErr bool
	}{
		{"human with actor", func(e *Event) {}, false},
		{"agent on behalf of a person", func(e *Event) { e.ActorKind, e.ActorPersonID, e.OnBehalfOf = Agent, "", person }, false},
		{"system with nobody", func(e *Event) { e.ActorKind, e.ActorPersonID = System, "" }, false},
		{"missing kind", func(e *Event) { e.Kind = "" }, true},
		{"missing subject type", func(e *Event) { e.SubjectType = "" }, true},
		{"human without actor", func(e *Event) { e.ActorPersonID = "" }, true},
		{"agent without on-behalf-of", func(e *Event) { e.ActorKind = Agent }, true},
		{"system with an actor", func(e *Event) { e.ActorKind = System }, true},
		{"unknown actor kind", func(e *Event) { e.ActorKind = "robot" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ok
			tt.change(&e)
			if err := e.validate(); (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestChainHash(t *testing.T) {
	prev := bytes.Repeat([]byte{1}, 32)
	body := []byte(`{"seq":1}`)
	h := chainHash(prev, body)

	if len(h) != 32 {
		t.Fatalf("hash length = %d, want 32", len(h))
	}
	if !bytes.Equal(h, chainHash(prev, body)) {
		t.Error("same input gave a different hash")
	}
	if bytes.Equal(h, chainHash(zeroHash, body)) {
		t.Error("changing the previous hash did not change the hash")
	}
	if bytes.Equal(h, chainHash(prev, []byte(`{"seq":2}`))) {
		t.Error("changing one character of the body did not change the hash")
	}
}
