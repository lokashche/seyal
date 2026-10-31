// SPDX-License-Identifier: AGPL-3.0-or-later

package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Problem is one break found in a chain.
type Problem struct {
	Seq    int64
	Reason string
}

// Report is the result of checking one tenant's chain.
type Report struct {
	Checked  int64 // events checked
	LastSeq  int64 // last event in the chain
	LastHash []byte
	Problems []Problem
}

// OK reports whether no problems were found.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Checkpoint is a point in a chain that was verified earlier. The zero value means "from the
// beginning".
type Checkpoint struct {
	Seq  int64
	Hash []byte
}

// Verify checks the current tenant's chain after the checkpoint. It recomputes every hash and
// checks the links, the numbering, that the columns match the hashed body, and the chain head.
// tx must come from db.WithTenant.
func Verify(ctx context.Context, tx pgx.Tx, from Checkpoint) (Report, error) {
	var rep Report
	prevSeq, prevHash := int64(0), zeroHash
	if from.Seq > 0 {
		var stored []byte
		err := tx.QueryRow(ctx,
			`SELECT hash FROM ledger WHERE tenant_id = current_tenant() AND seq = $1`, from.Seq).Scan(&stored)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			rep.Problems = append(rep.Problems, Problem{from.Seq, "the last verified event is missing"})
		case err != nil:
			return rep, err
		case !bytes.Equal(stored, from.Hash):
			rep.Problems = append(rep.Problems, Problem{from.Seq, "the last verified event has changed since"})
		}
		prevSeq, prevHash = from.Seq, from.Hash
	}

	rows, err := tx.Query(ctx, `
		SELECT tenant_id::text, seq, occurred_at, kind, subject_type, coalesce(subject_id::text, ''),
			actor_kind, coalesce(actor_person_id::text, ''), coalesce(on_behalf_of::text, ''),
			body, prev_hash, hash
		FROM ledger WHERE tenant_id = current_tenant() AND seq > $1 ORDER BY seq`, from.Seq)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	for rows.Next() {
		var c row
		if err := rows.Scan(&c.tenant, &c.seq, &c.at, &c.kind, &c.subjectType, &c.subjectID,
			&c.actorKind, &c.actor, &c.onBehalfOf, &c.body, &c.prevHash, &c.hash); err != nil {
			return rep, err
		}
		rep.Checked++
		add := func(reason string) { rep.Problems = append(rep.Problems, Problem{c.seq, reason}) }
		if c.seq != prevSeq+1 {
			add(fmt.Sprintf("expected event %d next: events are missing", prevSeq+1))
		}
		if !bytes.Equal(c.prevHash, prevHash) {
			add("does not link to the previous event")
		}
		if !bytes.Equal(c.hash, chainHash(c.prevHash, []byte(c.body))) {
			add("content does not match its hash")
		}
		if reason := c.mismatch(); reason != "" {
			add(reason)
		}
		prevSeq, prevHash = c.seq, c.hash
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}
	rep.LastSeq, rep.LastHash = prevSeq, prevHash

	// The head must point at the last event; otherwise events were removed from the end.
	var headSeq int64
	var headHash []byte
	err = tx.QueryRow(ctx,
		`SELECT last_seq, last_hash FROM ledger_heads WHERE tenant_id = current_tenant()`).Scan(&headSeq, &headHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if rep.LastSeq != 0 {
			rep.Problems = append(rep.Problems, Problem{rep.LastSeq, "the chain head is missing"})
		}
	case err != nil:
		return rep, err
	case headSeq != rep.LastSeq || !bytes.Equal(headHash, rep.LastHash):
		rep.Problems = append(rep.Problems, Problem{headSeq,
			fmt.Sprintf("the chain head does not match the last event (head %d, last %d)", headSeq, rep.LastSeq)})
	}
	return rep, nil
}

// LastCheckpoint returns the latest verified point of the current tenant's chain, or the zero
// Checkpoint if it was never verified.
func LastCheckpoint(ctx context.Context, tx pgx.Tx) (Checkpoint, error) {
	var cp Checkpoint
	err := tx.QueryRow(ctx, `
		SELECT seq, hash FROM ledger_checkpoints
		WHERE tenant_id = current_tenant() ORDER BY seq DESC LIMIT 1`).Scan(&cp.Seq, &cp.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, nil
	}
	return cp, err
}

// SaveCheckpoint records that the chain was verified up to rep.LastSeq.
func SaveCheckpoint(ctx context.Context, tx pgx.Tx, rep Report) error {
	if rep.LastSeq == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO ledger_checkpoints (tenant_id, seq, hash) VALUES (current_tenant(), $1, $2)
		ON CONFLICT (tenant_id, seq) DO NOTHING`, rep.LastSeq, rep.LastHash)
	return err
}

// row is one stored event, as read back for checking.
type row struct {
	tenant, kind, subjectType, subjectID, actorKind, actor, onBehalfOf, body string
	seq                                                                      int64
	at                                                                       time.Time
	prevHash, hash                                                           []byte
}

// mismatch reports which column differs from the hashed body, or "" if they all agree.
func (c row) mismatch() string {
	var b body
	if err := json.Unmarshal([]byte(c.body), &b); err != nil {
		return "body is not valid JSON"
	}
	checks := []struct {
		column string
		ok     bool
	}{
		{"tenant_id", b.Tenant == c.tenant},
		{"seq", b.Seq == c.seq},
		{"occurred_at", b.At.Equal(c.at)},
		{"kind", b.Kind == c.kind},
		{"subject_type", b.SubjectType == c.subjectType},
		{"subject_id", b.SubjectID == c.subjectID},
		{"actor_kind", string(b.ActorKind) == c.actorKind},
		{"actor_person_id", b.ActorPersonID == c.actor},
		{"on_behalf_of", b.OnBehalfOf == c.onBehalfOf},
	}
	for _, ch := range checks {
		if !ch.ok {
			return ch.column + " column differs from the hashed body"
		}
	}
	return ""
}
