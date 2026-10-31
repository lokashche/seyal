// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"testing"
	"time"
)

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		pattern string
		year    int
		seq     int64
		want    string
	}{
		{"J-{yyyy}-{seq:4}", 2026, 1, "J-2026-0001"},
		{"J-{yyyy}-{seq:4}", 2026, 12345, "J-2026-12345"},
		{"QS/{yy}/{seq:3}", 2026, 7, "QS/26/007"},
		{"{seq:1}", 2026, 42, "42"},
	}
	for _, tt := range tests {
		if got := FormatNumber(tt.pattern, tt.year, tt.seq); got != tt.want {
			t.Errorf("FormatNumber(%q, %d, %d) = %q, want %q", tt.pattern, tt.year, tt.seq, got, tt.want)
		}
	}
}

func TestFiscalYear(t *testing.T) {
	date := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	tests := []struct {
		date       string
		startMonth int
		want       int
	}{
		{"2026-03-15", 1, 2026}, // calendar year
		{"2026-12-31", 1, 2026},
		{"2026-03-31", 4, 2025}, // April start: still last year's fiscal year
		{"2026-04-01", 4, 2026},
		{"2027-01-10", 4, 2026},
	}
	for _, tt := range tests {
		if got := FiscalYear(date(tt.date), tt.startMonth); got != tt.want {
			t.Errorf("FiscalYear(%s, start %d) = %d, want %d", tt.date, tt.startMonth, got, tt.want)
		}
	}
}
