//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/audit"
	"seyal/internal/db"
	"seyal/internal/ledger"
)

func TestSettingsAccess(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug

	a := s.logIn(arjun)
	if code, _, _ := s.get(a, base+"/settings/people"); code != http.StatusForbidden {
		t.Errorf("non-admin opening Settings = %d, want 403", code)
	}
	if _, page, _ := s.get(a, base+"/"); strings.Contains(page, "/settings/") {
		t.Error("a non-admin sees the Settings link")
	}
	l := s.logIn(lakshmi)
	if _, page, _ := s.get(l, base+"/"); !strings.Contains(page, base+"/settings/people") {
		t.Error("the admin doesn't see the Settings link")
	}
	if code, page, _ := s.get(l, base+"/settings/people"); code != http.StatusOK || !strings.Contains(page, "Arjun Mehta") {
		t.Errorf("admin opening People = %d", code)
	}
	if code, _, _ := s.post(a, base+"/settings/roles", url.Values{"name": {"Sneaky"}}, nil); code != http.StatusForbidden {
		t.Errorf("non-admin adding a role = %d, want 403", code)
	}
}

func TestSettingsPeople(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)

	// Add Vikram as Sales; he gets a link to set his password and is logged in by it.
	code, page, _ := s.post(l, base+"/settings/people",
		url.Values{"name": {"Vikram Rao"}, "email": {"vikram@kaveri-demo.example"}, "role": {"sales"}}, nil)
	if code != http.StatusOK {
		t.Fatalf("add person = %d: %s", code, problem(page))
	}
	v := s.useLink(page, "vikram@kaveri-demo.example", "vikram-own-pass-1")

	t.Run("the same email can't be used twice", func(t *testing.T) {
		code, body, _ := s.post(l, base+"/settings/people",
			url.Values{"name": {"Someone"}, "email": {"vikram@kaveri-demo.example"}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "already uses") {
			t.Errorf("duplicate email = %d: %s", code, problem(body))
		}
	})

	vikramID := s.personID(t, "vikram@kaveri-demo.example")
	update := func(id string, form url.Values) (int, string) {
		code, body, _ := s.post(l, base+"/settings/people/"+id, form, nil)
		return code, body
	}

	t.Run("a role can be taken away and given back", func(t *testing.T) {
		update(vikramID, url.Values{"name": {"Vikram Rao"}, "email": {"vikram@kaveri-demo.example"}, "status": {"active"}})
		if s.holds(t, vikramID, "sales") {
			t.Error("sales still held after removing it")
		}
		update(vikramID, url.Values{"name": {"Vikram Rao"}, "email": {"vikram@kaveri-demo.example"}, "status": {"active"}, "role": {"sales"}})
		if !s.holds(t, vikramID, "sales") {
			t.Error("sales not held after giving it back")
		}
	})

	t.Run("disabling logs the person out at once", func(t *testing.T) {
		update(vikramID, url.Values{"name": {"Vikram Rao"}, "email": {"vikram@kaveri-demo.example"}, "status": {"disabled"}})
		if code, _, _ := s.get(v, base+"/"); code != http.StatusSeeOther {
			t.Errorf("disabled person's page = %d, want a redirect to login", code)
		}
	})

	t.Run("an admin can't lock themselves out", func(t *testing.T) {
		me := s.personID(t, lakshmi)
		code, body := update(me, url.Values{"name": {"Lakshmi Narayanan"}, "email": {lakshmi}, "status": {"active"}, "role": {"owner"}})
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "own admin role") {
			t.Errorf("removing own admin role = %d: %s", code, problem(body))
		}
	})

	t.Run("a new link sets a new password; the old one stops working", func(t *testing.T) {
		arjunID := s.personID(t, arjun)
		_, page, _ := s.post(l, base+"/settings/people/"+arjunID+"/link", nil, nil)
		old := s.passwords[arjun]
		s.useLink(page, arjun, "arjun-new-pass-1")
		if code, _, _ := s.post(s.browser(), base+"/login", url.Values{"email": {arjun}, "password": {old}}, nil); code != http.StatusUnauthorized {
			t.Errorf("old password = %d, want refused", code)
		}
		s.logIn(arjun)
	})

	t.Run("a new role", func(t *testing.T) {
		_, page, _ := s.post(l, base+"/settings/roles", url.Values{"name": {"Reviewer"}}, nil)
		if !strings.Contains(page, "Role Reviewer saved") || !strings.Contains(page, "reviewer") {
			t.Error("new role not shown")
		}
	})

	s.tablesMatchLedger(t)
}

func TestSettingsMasterData(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)

	t.Run("add a client, then use it in a job", func(t *testing.T) {
		_, page, _ := s.post(l, base+"/settings/master/client", url.Values{"name": {"Rasi Textiles"},
			"f.contact_person": {"Kavitha R"}, "f.email": {"accounts@rasi.example"}}, nil)
		if !strings.Contains(page, "Rasi Textiles saved") || !strings.Contains(page, "Kavitha R") {
			t.Fatal("new client not shown")
		}
		a := s.logIn(arjun)
		_, home, _ := s.get(a, base+"/")
		pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"New job for Rasi"}}, nil)
		page = s.send(a, h.Get("Location")+"/say", url.Values{"text": {"Rasi Textiles"}})
		if !strings.Contains(page, "What kind of work is it?") {
			t.Error("the new client wasn't accepted in the chat")
		}
	})

	t.Run("change a work type's standard fee and default maker", func(t *testing.T) {
		_, page, _ := s.post(l, base+"/settings/master/work_type", url.Values{"key": {"gst-returns"},
			"name": {"GST returns (monthly)"}, "f.standard_fee": {"6k"}, "f.maker": {s.people["rahul"]},
			"f.documents": {"Sales register\nPurchase register"}, "f.is_new": {"no"}}, nil)
		if !strings.Contains(page, "GST returns (monthly) saved") || !strings.Contains(page, "₹6,000") {
			t.Error("fee change not shown")
		}
	})

	t.Run("retire and bring back", func(t *testing.T) {
		_, page, _ := s.post(l, base+"/settings/master/client", url.Values{"key": {"ganga-traders"},
			"name": {"Ganga Traders"}, "action": {"retire"}}, nil)
		if !strings.Contains(page, "Ganga Traders retired") {
			t.Fatal("not retired")
		}
		a := s.logIn(arjun)
		_, home, _ := s.get(a, base+"/")
		pv := regexp.MustCompile(`name="playbook" value="([^"]+)"`).FindStringSubmatch(home)[1]
		_, _, h := s.post(a, base+"/tell", url.Values{"playbook": {pv}, "text": {"New job"}}, nil)
		_, item, _ := s.get(a, h.Get("Location"))
		if strings.Contains(item, ">Ganga Traders</button>") {
			t.Error("a retired client is still offered for new work")
		}
		_, page, _ = s.post(l, base+"/settings/master/client", url.Values{"key": {"ganga-traders"},
			"name": {"Ganga Traders"}, "action": {"restore"}}, nil)
		if !strings.Contains(page, "Ganga Traders is back in use") {
			t.Error("not brought back")
		}
	})

	t.Run("bad values are explained", func(t *testing.T) {
		code, body, _ := s.post(l, base+"/settings/master/work_type", url.Values{"key": {"gst-returns"},
			"name": {"GST returns (monthly)"}, "f.standard_fee": {"lots"}}, nil)
		if code != http.StatusUnprocessableEntity || !strings.Contains(problem(body), "Standard fee") {
			t.Errorf("bad fee = %d: %s", code, problem(body))
		}
	})

	t.Run("unknown kinds are not found", func(t *testing.T) {
		if code, _, _ := s.get(l, base+"/settings/master/spaceships"); code != http.StatusNotFound {
			t.Errorf("unknown kind = %d", code)
		}
	})

	s.tablesMatchLedger(t)
}

func (s *site) personID(t *testing.T, email string) string {
	t.Helper()
	var id string
	err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT id::text FROM persons WHERE login_email = $1", email).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *site) holds(t *testing.T, personID, role string) bool {
	t.Helper()
	var n int
	err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM person_roles
			WHERE person_id::text = $1 AND role_key = $2 AND revoked_at IS NULL`, personID, role).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func (s *site) tablesMatchLedger(t *testing.T) {
	t.Helper()
	err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
		rep, err := ledger.Verify(context.Background(), tx, ledger.Checkpoint{})
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Errorf("ledger problems: %v", rep.Problems)
		}
		problems, err := audit.CheckTables(context.Background(), tx)
		if len(problems) > 0 {
			t.Errorf("tables differ from the ledger: %v", problems)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
