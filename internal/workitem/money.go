// SPDX-License-Identifier: AGPL-3.0-or-later

package workitem

import (
	"strconv"
	"strings"
)

// currencySigns are the signs Money writes before an amount. Any other currency is written with its
// code and a space, e.g. "AED 1,250.00".
var currencySigns = map[string]string{"INR": "₹", "USD": "$", "EUR": "€", "GBP": "£"}

// Money shows an amount kept in minor units (paise, cents) with the business's currency, the way a
// message that leaves Seyal reads it: 1500000 in INR is "₹15,000.00", and 15000000 is "₹1,50,000.00"
// (rupees are grouped the Indian way: thousands, then twos). Other currencies are grouped in threes.
// It is used only for share texts (D91, decision 25 of the Phase 4 design): posts and titles keep
// today's plain "15000.00" (display), so nothing written before changes.
func Money(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	whole, cents := strconv.FormatInt(minor/100, 10), minor%100
	if currency == "INR" {
		whole = groupIndian(whole)
	} else {
		whole = group(whole, 3)
	}
	amount := whole + "." + strconv.FormatInt(100+cents, 10)[1:]
	if s, ok := currencySigns[currency]; ok {
		return sign + s + amount
	}
	if currency == "" {
		return sign + amount
	}
	return sign + currency + " " + amount
}

// group puts a comma before every size digits, from the right: "1234567" → "1,234,567".
func group(digits string, size int) string {
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%size == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return b.String()
}

// groupIndian groups the last three digits, then twos: "15000000" → "1,50,00,000".
func groupIndian(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
	return group(head, 2) + "," + tail
}
