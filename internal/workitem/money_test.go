// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import "testing"

func TestMoney(t *testing.T) {
	for _, c := range []struct {
		minor    int64
		currency string
		want     string
	}{
		{1500000, "INR", "₹15,000.00"},
		{15000000, "INR", "₹1,50,000.00"},
		{1234567890, "INR", "₹1,23,45,678.90"},
		{50005, "INR", "₹500.05"},
		{7, "INR", "₹0.07"},
		{-250000, "INR", "-₹2,500.00"},
		{1234567890, "USD", "$12,345,678.90"},
		{100000, "EUR", "€1,000.00"},
		{125000, "AED", "AED 1,250.00"},
		{125000, "", "1,250.00"},
	} {
		if got := Money(c.minor, c.currency); got != c.want {
			t.Errorf("Money(%d, %q) = %q; want %q", c.minor, c.currency, got, c.want)
		}
	}
}
