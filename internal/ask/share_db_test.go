//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ask_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/workitem"
)

// TestShareKeepsSuggestion: a share (item.shared, D91) changes nothing on the item, so it isn't "the
// item changed" for an open suggestion: Dev's spoken pass still works after Chitra shares the item.
func TestShareKeepsSuggestion(t *testing.T) {
	tm := newTeam(t)
	job := tm.withOps()
	tm.write(job.ID, tm.dev, "take it")
	r := tm.speak(job.ID, tm.dev, "pass to Ben Sample, he is free")
	if r.Item.AssigneeID != tm.dev || !strings.Contains(said(r), "Pass J-2026-0001 to Ben Sample") {
		t.Fatalf("a spoken pass: with %q, Seyal said %q; want a suggestion", r.Item.AssigneeID, said(r))
	}
	q := tm.choosing(job.ID, tm.dev)
	if len(q) != 2 {
		t.Fatalf("Dev's buttons = %+v; want Yes and No", q)
	}
	tm.in(func(tx pgx.Tx) error {
		recorded, err := workitem.RecordShare(tm.ctx, tx, job.ID, ledger.PersonActor(tm.chitra), workitem.ShareRecord{
			ID: playbook.BuiltinShare, Route: workitem.RouteCopy, Text: "J-2026-0001: please have a look",
			Attempt: "0b6c3f9e-2d4a-4c1b-9e8f-7a6b5c4d3e2f"}, workitem.ShareEnv{Link: "https://seyal.example/t/x/items/" + job.ID}, nil)
		if err == nil && !recorded {
			t.Error("Chitra's share wasn't recorded")
		}
		return err
	})
	if got := tm.choosing(job.ID, tm.dev); len(got) != 2 {
		t.Errorf("after a share, Dev's buttons = %+v; want still Yes and No", got)
	}
	if r = tm.press(job.ID, tm.dev, q[0]); r.Item.AssigneeID != tm.benS {
		t.Errorf("Dev's Yes after a share: with %q, Seyal said %q; want passed to Ben Sample", r.Item.AssigneeID, said(r))
	}
	tm.checkLedger()
}
