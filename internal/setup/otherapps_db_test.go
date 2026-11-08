//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package setup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/db"
	"seyal/internal/ledger"
	"seyal/internal/playbook"
	"seyal/internal/send"
	"seyal/internal/setup"
	"seyal/internal/testdb"
)

// TestImportOtherApps: the set-up file's other_apps (D95). The import writes the row with a new
// signing secret, given back once; importing again keeps it and changes nothing; --new-secret
// replaces it; a change and a removal are ledger events. No ledger event, and nothing `seyal verify`
// says, holds the address or the secret. A row changed outside Seyal is found by verify.
func TestImportOtherApps(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	lib, err := playbook.LoadLibrary("../testdata/playbooks", "../../examples/masters")
	if err != nil {
		t.Fatal(err)
	}
	f, err := setup.Load("../../examples/seed/demo.yaml", lib)
	if err != nil {
		t.Fatal(err)
	}
	f.Business.Slug = "test-" + strings.ToLower(rand.Text()[:10])
	for i, p := range f.People {
		f.People[i].Email = p.Key + "@" + f.Business.Slug + ".kaveri-demo.example"
	}
	const marker = "MARKER7Q2Z" // in the address: it must never show up anywhere but other_apps
	f.OtherApps[0].URL = "https://sheet.kaveri-demo.example/macros/s/" + marker + "/exec"

	first, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	tenant := first.TenantID
	in := func(pool db.Beginner, fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := db.WithTenant(ctx, pool, tenant, fn); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() send.Target {
		t.Helper()
		var got send.Target
		in(app, func(tx pgx.Tx) error {
			l, err := send.Find(ctx, tx, "jobs-sheet")
			got = l.Target
			return err
		})
		return got
	}
	// clean checks verify: the ledger and every table match, and nothing it says holds a secret.
	verify := func(secret string) []audit.Problem {
		t.Helper()
		var problems []audit.Problem
		in(owner, func(tx pgx.Tx) error {
			r, err := ledger.Verify(ctx, tx, ledger.Checkpoint{})
			if err != nil || !r.OK() {
				t.Fatalf("ledger verify = %+v, %v", r, err)
			}
			problems, err = audit.CheckTables(ctx, tx)
			return err
		})
		for _, p := range problems {
			if strings.Contains(p.String(), marker) || strings.Contains(p.String(), secret) {
				t.Errorf("verify says %q, which holds the address or the secret", p)
			}
		}
		return problems
	}
	// ledgerClean checks that no event of the business holds the address or a secret.
	ledgerClean := func(secrets ...string) {
		t.Helper()
		in(app, func(tx pgx.Tx) error {
			var n int
			err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE body LIKE '%' || $1 || '%' OR body LIKE '%whsec\_%'`, marker).Scan(&n)
			if n != 0 {
				t.Errorf("%d ledger events hold the address or a secret", n)
			}
			for _, s := range secrets {
				if err == nil {
					err = tx.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE strpos(body, $1) > 0`, s[len("whsec_"):]).Scan(&n)
				}
				if n != 0 {
					t.Errorf("%d ledger events hold a signing secret", n)
				}
			}
			return err
		})
	}

	secret := first.Secrets["jobs-sheet"]
	if !strings.HasPrefix(secret, "whsec_") || len(first.Secrets) != 1 {
		t.Fatalf("first import's secrets = %v, want one for jobs-sheet", first.Secrets)
	}
	got := stored()
	if got.Secret != secret || got.URL != f.OtherApps[0].URL || got.Expect != send.Expect2xx || got.Label != "Jobs sheet (demo)" ||
		len(got.Fields) != 5 || got.Paused {
		t.Errorf("stored = %+v", got)
	}
	in(app, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE kind = 'business.updated'
			AND body::jsonb #>> '{payload,changes,other_app:jobs-sheet,new,secret}' = 'new'
			AND body::jsonb #>> '{payload,changes,other_app:jobs-sheet,new,address_ends}' = '…/exec'
			AND body::jsonb #>> '{payload,changes,other_app:jobs-sheet,new,host}' = 'sheet.kaveri-demo.example'
			AND body::jsonb #>> '{payload,changes,other_app:jobs-sheet,new,sha256}' = $1`, got.Hash()).Scan(&n)
		if n != 1 {
			t.Errorf("%d events recording the new other app, want 1", n)
		}
		return err
	})
	ledgerClean(secret)
	if p := verify(secret); len(p) != 0 {
		t.Errorf("verify after the import: %v", p)
	}

	// The app may only read the table.
	err = db.WithTenant(ctx, app, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE other_apps SET url = 'https://evil.example/x'`)
		return err
	})
	if err == nil {
		t.Error("the app role could change an other app's address")
	}

	// Again: the same secret, nothing to show, no change.
	again, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changes != 0 || len(again.Secrets) != 0 || stored().Secret != secret {
		t.Errorf("second import: %d changes, secrets %v; want none and the same secret", again.Changes, again.Secrets)
	}

	// --new-secret: a new one, shown once, recorded as replaced.
	f.NewSecrets = []string{"jobs-sheet"}
	if err := f.Check(lib); err != nil {
		t.Fatal(err)
	}
	replaced, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	f.NewSecrets = nil
	newSecret := replaced.Secrets["jobs-sheet"]
	if newSecret == "" || newSecret == secret || stored().Secret != newSecret || replaced.Changes != 1 {
		t.Errorf("--new-secret: %d changes, new secret set %v, same as before %v", replaced.Changes, newSecret != "", newSecret == secret)
	}
	in(app, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE
			body::jsonb #>> '{payload,changes,other_app:jobs-sheet,new,secret}' = 'replaced'`).Scan(&n)
		if n != 1 {
			t.Errorf("%d events recording the replaced secret, want 1", n)
		}
		return err
	})

	// A changed field list is a change; the secret stays.
	f.OtherApps[0].Fields = []string{"client", "fee_agreed"}
	changed, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Changes != 1 || len(changed.Secrets) != 0 || stored().Secret != newSecret || len(stored().Fields) != 2 {
		t.Errorf("changed fields: %+v", changed)
	}
	ledgerClean(secret, newSecret)
	if p := verify(newSecret); len(p) != 0 {
		t.Errorf("verify after the changes: %v", p)
	}

	// Changed outside Seyal (with the owner connection, as a person with database access could):
	// verify finds it, naming only the app.
	in(owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE other_apps SET url = 'https://sheet.kaveri-demo.example/macros/s/OTHER/exec'`)
		return err
	})
	p := verify(newSecret)
	if len(p) != 1 || p[0].Table != "other_apps" || p[0].ID != "jobs-sheet" || !strings.Contains(p[0].Reason, "changed outside Seyal") {
		t.Errorf("verify after a change by hand: %v", p)
	}
	// An import doesn't paper over it: the row isn't what the ledger last recorded, so it is refused.
	if _, err := setup.Import(ctx, owner, app, f); err == nil || !strings.Contains(err.Error(), "changed outside Seyal") ||
		strings.Contains(err.Error(), marker) {
		t.Errorf("import over a row changed by hand: %v", err)
	}
	in(owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE other_apps SET url = $1`, f.OtherApps[0].URL)
		return err
	})

	// Only the secret changed by hand: a later import must not keep it and record it as "kept" (which
	// would hide the change from verify). It is refused; --new-secret replaces it.
	in(owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE other_apps SET secret = 'whsec_byhand0000', label = 'By hand'`)
		return err
	})
	if _, err := setup.Import(ctx, owner, app, f); err == nil || !strings.Contains(err.Error(), "--new-secret jobs-sheet") {
		t.Errorf("import over a secret changed by hand: %v", err)
	}
	f.NewSecrets = []string{"jobs-sheet"}
	fixed, err := setup.Import(ctx, owner, app, f)
	f.NewSecrets = nil
	if err != nil {
		t.Fatal(err)
	}
	newSecret = fixed.Secrets["jobs-sheet"]
	if newSecret == "" || stored().Secret != newSecret || stored().Label != "Jobs sheet (demo)" {
		t.Errorf("--new-secret after a change by hand: %+v", fixed)
	}
	if p := verify(newSecret); len(p) != 0 {
		t.Errorf("verify after --new-secret: %v", p)
	}

	// A new address ("HTTPS://" is accepted and stored as "https://"): the last test reached the old
	// address, so it is forgotten; an admin's pause stays.
	var someone, tested string
	for _, id := range first.People {
		someone = id
		break
	}
	in(app, func(tx pgx.Tx) error {
		if _, err := send.Pause(ctx, tx, "jobs-sheet", true, someone); err != nil {
			return err
		}
		before, err := send.Find(ctx, tx, "jobs-sheet")
		if err != nil {
			return err
		}
		tested = before.Hash()
		return send.RecordTest(ctx, tx, "jobs-sheet", tested, someone, "answered OK")
	})
	f.OtherApps[0].URL = "HTTPS://sheet.kaveri-demo.example/macros/s/" + marker + "/v2/exec"
	if err := f.Check(lib); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Import(ctx, owner, app, f); err != nil {
		t.Fatal(err)
	}
	var l send.Listed
	in(app, func(tx pgx.Tx) error {
		l, err = send.Find(ctx, tx, "jobs-sheet")
		return err
	})
	if !strings.HasPrefix(l.URL, "https://sheet.") || !l.State.TestedAt.IsZero() || l.State.TestResult != "" || !l.State.Paused() {
		t.Errorf("after a new address: url scheme %q, last test %v %q, paused %v", l.URL[:8], l.State.TestedAt, l.State.TestResult, l.State.Paused())
	}
	// A test sent to the old address that finishes after the import is not kept as the new set-up's
	// last test.
	in(app, func(tx pgx.Tx) error {
		if err := send.RecordTest(ctx, tx, "jobs-sheet", tested, someone, "answered OK"); !errors.Is(err, send.ErrChanged) {
			t.Errorf("a test of the old set-up kept: %v", err)
		}
		return nil
	})
	in(app, func(tx pgx.Tx) error {
		l, err = send.Find(ctx, tx, "jobs-sheet")
		return err
	})
	if !l.State.TestedAt.IsZero() || l.State.TestResult != "" {
		t.Errorf("a late test of the old address shows as the last test: %v %q", l.State.TestedAt, l.State.TestResult)
	}
	tested = l.Hash()
	in(app, func(tx pgx.Tx) error { return send.RecordTest(ctx, tx, "jobs-sheet", tested, someone, "answered OK") })

	// Left out of the file: the row goes, the ledger says so, the import names it (its secret is
	// gone), and its last test is forgotten.
	f.OtherApps = nil
	removed, err := setup.Import(ctx, owner, app, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.RemovedApps) != 1 || removed.RemovedApps[0] != "jobs-sheet" {
		t.Errorf("removed apps = %v, want [jobs-sheet]", removed.RemovedApps)
	}
	in(owner, func(tx pgx.Tx) error {
		var tested int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM other_app_state WHERE tested_at IS NOT NULL`).Scan(&tested)
		if tested != 0 {
			t.Errorf("a removed app keeps its last test")
		}
		return err
	})
	in(app, func(tx pgx.Tx) error {
		all, err := send.Targets(ctx, tx)
		if len(all) != 0 {
			t.Errorf("%d other apps after leaving it out of the file, want 0", len(all))
		}
		return err
	})
	// A test that was on its way when the app was removed leaves nothing behind.
	in(app, func(tx pgx.Tx) error {
		if err := send.RecordTest(ctx, tx, "jobs-sheet", tested, someone, "answered OK"); !errors.Is(err, send.ErrChanged) {
			t.Errorf("a test of a removed app kept: %v", err)
		}
		return nil
	})
	in(owner, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM other_app_state WHERE tested_at IS NOT NULL`).Scan(&n)
		if n != 0 {
			t.Errorf("a late test of a removed app left a last test behind")
		}
		return err
	})
	if removed.Changes != 1 {
		t.Errorf("removing: %d changes, want 1", removed.Changes)
	}
	if p := verify(newSecret); len(p) != 0 {
		t.Errorf("verify after the removal: %v", p)
	}
}
