//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/db"
	"seyal/internal/engine"
	"seyal/internal/ledger"
	"seyal/internal/setup"
	"seyal/internal/workitem"
)

// TestPeriodByHand (D80's loose end): a Monthly return started by hand has no period yet, so its title
// is just the client's name (no dangling "—"), and people may fill in Period in the side panel like any
// other field. On an item the schedule started, Period stays fixed: the schedule filled it.
func TestPeriodByHand(t *testing.T) {
	s := newDemoSite(t)
	ctx := context.Background()
	base := "/t/" + s.slug
	var err error
	if s.passwords["chitra"], err = setup.NewPassword(ctx, s.app, s.mail("chitra"), s.tenant); err != nil {
		t.Fatal(err)
	}
	var byHand, scheduled workitem.Item
	err = db.WithTenant(ctx, s.app, s.tenant, func(tx pgx.Tx) error {
		var pv string
		if err := tx.QueryRow(ctx, `SELECT current_version_id::text FROM tenant_playbooks WHERE playbook_id = 'monthly-return'`).Scan(&pv); err != nil {
			return err
		}
		var err error
		if byHand, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "chat",
			By: ledger.PersonActor(s.people["chitra"]), Fields: engine.Fields{"client": "deccan-motors"}}); err != nil {
			return err
		}
		scheduled, err = workitem.Start(ctx, tx, workitem.NewItem{PlaybookVersionID: pv, Via: "schedule",
			Ref: "monthly-return:m2026-11:sundaram-textiles", By: ledger.SystemActor, Period: "Nov 2026",
			Reason: "Monthly schedule: Nov 2026, for Sundaram Textiles",
			Fields: engine.Fields{"client": "sundaram-textiles", "period": "Nov 2026"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if byHand.Title != "Deccan Motors" || scheduled.Title != "Sundaram Textiles — Nov 2026" {
		t.Errorf("titles = %q and %q, want %q and %q", byHand.Title, scheduled.Title, "Deccan Motors", "Sundaram Textiles — Nov 2026")
	}

	c := s.logIn("chitra")
	periodForm := `<input type="hidden" name="key" value="period">`
	if _, page, _ := s.get(c, base+"/items/"+byHand.ID); !strings.Contains(page, periodForm) {
		t.Errorf("Period should be open to fill in on an item started by hand:\n%s", problem(page))
	}
	page := html.UnescapeString(s.send(c, base+"/items/"+byHand.ID+"/field", url.Values{"key": {"period"}, "value": {"Nov 2026"}}))
	if !strings.Contains(page, "Deccan Motors — Nov 2026") {
		t.Errorf("after filling in Period the title should be \"Deccan Motors — Nov 2026\":\n%s", problem(page))
	}

	if _, page, _ := s.get(c, base+"/items/"+scheduled.ID); strings.Contains(page, periodForm) {
		t.Error("Period should be fixed on an item the schedule started")
	}
	code, body, _ := s.post(c, base+"/items/"+scheduled.ID+"/field", url.Values{"key": {"period"}, "value": {"Oct 2026"}}, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "filled in automatically") {
		t.Errorf("changing the period the schedule filled = %d: %s", code, problem(body))
	}
	s.tablesMatchLedger(t)
}
