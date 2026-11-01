//go:build db

// SPDX-License-Identifier: AGPL-3.0-or-later

package ai_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"seyal/internal/ai"
	"seyal/internal/db"
	"seyal/internal/testdb"
)

func TestCallsAreRecordedAndReported(t *testing.T) {
	ctx := context.Background()
	owner, app := testdb.Connect(t)
	tenant, person := testdb.NewTenant(t, owner, "AI Test")

	fake := &ai.Fake{Usage: ai.Usage{Input: 1000, Cached: 3000, Output: 600}}
	s := &ai.Setup{USDtoINR: 88}
	s.Set(ai.Understand, ai.Choice{Name: "fake:demo", Model: fake, Price: ai.Price{Input: 4, Cached: 0.2, Output: 20}})
	in := func(fn func(tx pgx.Tx) error) error { return db.WithTenant(ctx, app, tenant, fn) }

	// Two good calls, one failure, and one for a purpose that is off.
	for i := 0; i < 2; i++ {
		if err := in(func(tx pgx.Tx) error {
			_, err := s.Call(ctx, tx, ai.Understand, ai.Request{Input: "hello"}, ai.Link{PersonID: person})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	fake.Answer = func(ai.Request) (string, error) { return "", ai.ErrFakeDown }
	err := in(func(tx pgx.Tx) error {
		_, err := s.Call(ctx, tx, ai.Understand, ai.Request{Input: "hello"}, ai.Link{PersonID: person})
		if !errors.Is(err, ai.ErrFakeDown) {
			t.Errorf("failed call = %v, want the provider's error", err)
		}
		return nil // keep the record of the failed call
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := in(func(tx pgx.Tx) error {
		_, err := s.Call(ctx, tx, ai.Draft, ai.Request{}, ai.Link{})
		if !errors.Is(err, ai.ErrOff) {
			t.Errorf("call for a purpose that is off = %v, want ErrOff", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var rep ai.Report
	if err := in(func(tx pgx.Tx) error {
		var err error
		rep, err = ai.MakeReport(ctx, tx, time.Now().Add(-time.Hour), time.UTC)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rep.Total.Calls != 3 || rep.Total.Failed != 1 || rep.Total.Output != 1800 {
		t.Errorf("total = %+v, want 3 calls, 1 failed, 1800 output tokens", rep.Total)
	}
	if want := 3 * 0.0166; math.Abs(rep.Total.CostUSD-want) > 1e-6 {
		t.Errorf("cost = %v, want %v", rep.Total.CostUSD, want)
	}
	if len(rep.ByPerson) != 1 || rep.ByPerson[0].Label != "Test Person" || len(rep.ByDay) != 1 {
		t.Errorf("by person %+v, by day %+v", rep.ByPerson, rep.ByDay)
	}

	t.Run("another business can't see the calls", func(t *testing.T) {
		other, _ := testdb.NewTenant(t, owner, "AI Test B")
		err := db.WithTenant(ctx, app, other, func(tx pgx.Tx) error {
			r, err := ai.MakeReport(ctx, tx, time.Now().Add(-time.Hour), time.UTC)
			if err == nil && r.Total.Calls != 0 {
				t.Errorf("other business sees %d calls", r.Total.Calls)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
