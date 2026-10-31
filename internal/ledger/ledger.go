// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ledger is Seyal's append-only, hash-chained event log. Each tenant has its own chain:
// every event stores the hash of the previous one, so changing any past event breaks the chain.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ActorKind says in what capacity an event was recorded.
type ActorKind string

const (
	Human  ActorKind = "human"  // a person acted directly
	Agent  ActorKind = "agent"  // Seyal acted on behalf of a person
	System ActorKind = "system" // Seyal acted on its own, e.g. a rule fired
)

// Event is something to record in the ledger.
type Event struct {
	Kind          string // what happened, e.g. "item.started"
	SubjectType   string // what it happened to, e.g. "work_item"
	SubjectID     string // optional id of the subject
	ActorKind     ActorKind
	ActorPersonID string // the person who acted (Human only)
	OnBehalfOf    string // the person an agent acted for (Agent only)
	Payload       any    // details, stored as JSON
}

// Actor says who did something and in what capacity.
type Actor struct {
	Kind       ActorKind
	PersonID   string // Human only
	OnBehalfOf string // Agent only
}

// PersonActor is a person acting directly.
func PersonActor(personID string) Actor { return Actor{Kind: Human, PersonID: personID} }

// AgentActor is Seyal acting on behalf of a person.
func AgentActor(onBehalfOf string) Actor { return Actor{Kind: Agent, OnBehalfOf: onBehalfOf} }

// SystemActor is Seyal acting on its own, e.g. when a rule fires.
var SystemActor = Actor{Kind: System}

// By returns a copy of e recorded as done by a.
func (e Event) By(a Actor) Event {
	e.ActorKind, e.ActorPersonID, e.OnBehalfOf = a.Kind, a.PersonID, a.OnBehalfOf
	return e
}

// Recorded is an event as stored in the chain.
type Recorded struct {
	Seq        int64
	OccurredAt time.Time
	Body       string // the exact JSON text that was hashed
	PrevHash   []byte
	Hash       []byte
}

// zeroHash is the "previous hash" of a tenant's first event.
var zeroHash = make([]byte, sha256.Size)

// body is the hashed content of an event. Its field order is fixed, so the JSON is
// deterministic.
type body struct {
	V             int             `json:"v"` // body format version
	Tenant        string          `json:"tenant"`
	Seq           int64           `json:"seq"`
	At            time.Time       `json:"at"`
	Kind          string          `json:"kind"`
	SubjectType   string          `json:"subject_type"`
	SubjectID     string          `json:"subject_id,omitempty"`
	ActorKind     ActorKind       `json:"actor_kind"`
	ActorPersonID string          `json:"actor,omitempty"`
	OnBehalfOf    string          `json:"on_behalf_of,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// Append adds e to the end of the current tenant's chain. tx must come from db.WithTenant.
func Append(ctx context.Context, tx pgx.Tx, e Event) (Recorded, error) {
	if err := e.validate(); err != nil {
		return Recorded{}, err
	}
	payload := json.RawMessage("{}")
	if e.Payload != nil {
		var err error
		if payload, err = json.Marshal(e.Payload); err != nil {
			return Recorded{}, fmt.Errorf("ledger: payload: %w", err)
		}
	}

	// Create this tenant's chain head on first use, then lock it until the transaction ends.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_heads (tenant_id, last_seq, last_hash)
		VALUES (current_tenant(), 0, $1)
		ON CONFLICT (tenant_id) DO NOTHING`, zeroHash); err != nil {
		return Recorded{}, fmt.Errorf("ledger: create head: %w", err)
	}
	var tenantID string
	var lastSeq int64
	var lastHash []byte
	if err := tx.QueryRow(ctx, `
		SELECT tenant_id::text, last_seq, last_hash FROM ledger_heads
		WHERE tenant_id = current_tenant() FOR UPDATE`).Scan(&tenantID, &lastSeq, &lastHash); err != nil {
		return Recorded{}, fmt.Errorf("ledger: lock head: %w", err)
	}

	r := Recorded{
		Seq:        lastSeq + 1,
		OccurredAt: time.Now().UTC().Truncate(time.Microsecond), // Postgres keeps microseconds
		PrevHash:   lastHash,
	}
	data, err := json.Marshal(body{
		V: 1, Tenant: tenantID, Seq: r.Seq, At: r.OccurredAt,
		Kind: e.Kind, SubjectType: e.SubjectType, SubjectID: e.SubjectID,
		ActorKind: e.ActorKind, ActorPersonID: e.ActorPersonID, OnBehalfOf: e.OnBehalfOf,
		Payload: payload,
	})
	if err != nil {
		return Recorded{}, fmt.Errorf("ledger: body: %w", err)
	}
	r.Body = string(data)
	r.Hash = chainHash(lastHash, data)

	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger (tenant_id, seq, occurred_at, kind, subject_type, subject_id,
			actor_kind, actor_person_id, on_behalf_of, body, prev_hash, hash)
		VALUES (current_tenant(), $1, $2, $3, $4, nullif($5::text, '')::uuid,
			$6, nullif($7::text, '')::uuid, nullif($8::text, '')::uuid, $9, $10, $11)`,
		r.Seq, r.OccurredAt, e.Kind, e.SubjectType, e.SubjectID,
		string(e.ActorKind), e.ActorPersonID, e.OnBehalfOf, r.Body, r.PrevHash, r.Hash); err != nil {
		return Recorded{}, fmt.Errorf("ledger: insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ledger_heads SET last_seq = $1, last_hash = $2
		WHERE tenant_id = current_tenant()`, r.Seq, r.Hash); err != nil {
		return Recorded{}, fmt.Errorf("ledger: move head: %w", err)
	}
	return r, nil
}

// chainHash links an event to the previous one: sha256(prevHash || body).
func chainHash(prevHash, body []byte) []byte {
	h := sha256.New()
	h.Write(prevHash)
	h.Write(body)
	return h.Sum(nil)
}

// validate checks that the event says clearly who acted and in what capacity.
func (e Event) validate() error {
	if e.Kind == "" {
		return errors.New("ledger: event kind is required")
	}
	if e.SubjectType == "" {
		return errors.New("ledger: subject type is required")
	}
	switch e.ActorKind {
	case Human:
		if e.ActorPersonID == "" || e.OnBehalfOf != "" {
			return errors.New("ledger: a human event needs an actor person and no on-behalf-of")
		}
	case Agent:
		if e.OnBehalfOf == "" || e.ActorPersonID != "" {
			return errors.New("ledger: an agent event needs on-behalf-of and no actor person")
		}
	case System:
		if e.ActorPersonID != "" || e.OnBehalfOf != "" {
			return errors.New("ledger: a system event has no actor person and no on-behalf-of")
		}
	default:
		return fmt.Errorf("ledger: unknown actor kind %q", e.ActorKind)
	}
	return nil
}
