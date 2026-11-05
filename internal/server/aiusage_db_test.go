//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/db"
)

func TestAIUsagePage(t *testing.T) {
	s := newSite(t)
	base := "/t/" + s.slug
	l := s.logIn(lakshmi)

	t.Run("with AI off", func(t *testing.T) {
		code, page, _ := s.get(l, base+"/settings/ai")
		if code != http.StatusOK || !strings.Contains(page, "No AI calls yet") || strings.Count(page, ">off<") != len(ai.Purposes)+1 { // +1: speech to text
			t.Errorf("AI page with AI off = %d", code)
		}
	})

	t.Run("costs of recorded calls, in dollars and rupees", func(t *testing.T) {
		fake := &ai.Fake{Usage: ai.Usage{Input: 1000, Cached: 3000, Output: 600}}
		s.ai.Set(ai.Understand, ai.Choice{Name: "fake:demo", Model: fake, Price: ai.Price{Input: 4, Cached: 0.2, Output: 20}})
		err := db.WithTenant(context.Background(), testdbApp(t), s.tenant, func(tx pgx.Tx) error {
			for i := 0; i < 10; i++ {
				if _, err := s.ai.Call(context.Background(), tx, ai.Understand, ai.Request{Input: "hello"},
					ai.Link{PersonID: s.people["arjun"]}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, page, _ := s.get(l, base+"/settings/ai")
		// 10 calls × $0.0166 = $0.17 (rounded), × ₹88 = ₹14.61
		for _, want := range []string{"fake:demo", "Arjun Mehta", "<td>10</td>", "$0.17", "₹14.61", "6,000"} {
			if !strings.Contains(page, want) {
				t.Errorf("AI page is missing %q", want)
			}
		}
	})

	t.Run("admins only", func(t *testing.T) {
		if code, _, _ := s.get(s.logIn(arjun), base+"/settings/ai"); code != http.StatusForbidden {
			t.Errorf("non-admin = %d, want 403", code)
		}
	})
}
