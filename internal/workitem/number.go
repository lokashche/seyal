// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workitem stores work items and applies the engine's decisions to them. Every change
// is written to the ledger in the same transaction as the change itself.
package workitem

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var seqToken = regexp.MustCompile(`\{seq:([1-9])\}`)

// FormatNumber fills a numbering pattern such as "J-{yyyy}-{seq:4}". {seq:4} pads the sequence
// to at least 4 digits; bigger numbers simply get longer ("J-2026-12345").
func FormatNumber(pattern string, year int, seq int64) string {
	s := strings.NewReplacer(
		"{yyyy}", fmt.Sprintf("%04d", year),
		"{yy}", fmt.Sprintf("%02d", year%100),
	).Replace(pattern)
	return seqToken.ReplaceAllStringFunc(s, func(token string) string {
		width := int(token[len("{seq:")] - '0')
		return fmt.Sprintf("%0*d", width, seq)
	})
}

// FiscalYear returns the year a fiscal year is named after: the calendar year in which it
// starts. With startMonth 4 (April), 15 March 2026 belongs to the fiscal year that started in
// April 2025, so it returns 2025. t should already be in the tenant's time zone.
func FiscalYear(t time.Time, startMonth int) int {
	if int(t.Month()) < startMonth {
		return t.Year() - 1
	}
	return t.Year()
}

// TakeNumber takes the next number from the current tenant's counter called key, starting at 1.
// The counter row stays locked until the transaction ends, so numbers are never given twice,
// and a transaction that fails gives its number back, so there are no gaps.
// tx must come from db.WithTenant.
func TakeNumber(ctx context.Context, tx pgx.Tx, key string) (int64, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO counters (tenant_id, key) VALUES (current_tenant(), $1)
		ON CONFLICT (tenant_id, key) DO NOTHING`, key); err != nil {
		return 0, fmt.Errorf("workitem: create counter: %w", err)
	}
	var n int64
	err := tx.QueryRow(ctx, `
		UPDATE counters SET next_value = next_value + 1
		WHERE tenant_id = current_tenant() AND key = $1
		RETURNING next_value - 1`, key).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("workitem: take number: %w", err)
	}
	return n, nil
}
