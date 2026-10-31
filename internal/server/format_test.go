// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import "testing"

func TestMoney(t *testing.T) {
	tests := []struct {
		minor    float64
		currency string
		want     string
	}{
		{1200000, "INR", "₹12,000"},
		{120000000, "INR", "₹12,00,000"},
		{1234567, "INR", "₹12,345.67"},
		{500, "INR", "₹5"},
		{120000000, "USD", "$1,200,000"},
		{-5050, "EUR", "−€50.50"},
		{100, "AED", "AED 1"},
	}
	for _, tt := range tests {
		if got := money(tt.minor, tt.currency); got != tt.want {
			t.Errorf("money(%v, %s) = %q, want %q", tt.minor, tt.currency, got, tt.want)
		}
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"job-evaluation": "Job evaluation", "pending_approval": "Pending approval", "": ""} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
