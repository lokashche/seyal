// SPDX-License-Identifier: AGPL-3.0-or-later

package ai

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Line is one row of the cost report.
type Line struct {
	Label                 string // a purpose, a model, a person's name or a day
	Calls, Failed         int
	Input, Cached, Output int64
	CostUSD               float64
}

// Report is AI use since a moment, grouped several ways.
type Report struct {
	Since                        time.Time
	Total                        Line
	ByPurpose, ByModel, ByPerson []Line
	ByDay                        []Line
}

// MakeReport sums the recorded calls since a moment; days are counted in the given time zone.
// tx must come from db.WithTenant.
func MakeReport(ctx context.Context, tx pgx.Tx, since time.Time, zone *time.Location) (Report, error) {
	if zone == nil {
		zone = time.UTC
	}
	r := Report{Since: since}
	// label is one of the fixed expressions below, never text from a request.
	group := func(label string) ([]Line, error) {
		rows, _ := tx.Query(ctx, `
			SELECT `+label+` AS label, count(*), count(*) FILTER (WHERE NOT c.ok),
				coalesce(sum(c.input_tokens), 0), coalesce(sum(c.cached_tokens), 0), coalesce(sum(c.output_tokens), 0),
				coalesce(sum(c.cost_usd), 0)::float8
			FROM ai_calls c
			LEFT JOIN persons p ON p.tenant_id = c.tenant_id AND p.id = c.person_id
			WHERE c.tenant_id = current_tenant() AND c.created_at >= $1 AND $2::text <> ''
			GROUP BY 1 ORDER BY 1`, since, zone.String())
		return pgx.CollectRows(rows, pgx.RowToStructByPos[Line])
	}
	var err error
	if r.ByPurpose, err = group("c.purpose"); err != nil {
		return r, err
	}
	if r.ByModel, err = group("c.model"); err != nil {
		return r, err
	}
	if r.ByPerson, err = group("coalesce(p.display_name, 'Seyal (no person)')"); err != nil {
		return r, err
	}
	if r.ByDay, err = group("to_char(c.created_at AT TIME ZONE $2, 'YYYY-MM-DD')"); err != nil {
		return r, err
	}
	for _, l := range r.ByPurpose {
		r.Total.Calls += l.Calls
		r.Total.Failed += l.Failed
		r.Total.Input += l.Input
		r.Total.Cached += l.Cached
		r.Total.Output += l.Output
		r.Total.CostUSD += l.CostUSD
	}
	r.Total.Label = "Total"
	return r, nil
}
