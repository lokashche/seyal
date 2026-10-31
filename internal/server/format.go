// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"math"
	"strconv"
	"strings"
	"time"

	"seyal/internal/playbook"
)

// show turns a stored field value into what people read: names instead of ids, money with the
// business's currency, dates as "15 Nov 2026". An empty value shows as "".
func (v *view) show(f playbook.Field, value any) string {
	switch x := value.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			if s := v.show(playbook.Field{Key: f.Key, Type: f.Of}, item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case string:
		switch f.Type {
		case "person":
			if name, ok := v.names[x]; ok {
				return name
			}
		case "master":
			if name, ok := v.masters[f.Kind+"/"+x]; ok {
				return name
			}
		case "date":
			if t, err := time.Parse(time.DateOnly, x); err == nil {
				return t.Format("2 Jan 2006")
			}
		}
		return x
	case float64:
		if f.Type == "money" {
			return money(x, v.c.tenant.Currency)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	}
	return ""
}

var symbols = map[string]string{"INR": "₹", "USD": "$", "EUR": "€", "GBP": "£"}

// money shows an amount stored in minor units: 1200000 in INR is "₹12,000", 1234567 is
// "₹12,345.67". Rupees are grouped the Indian way (₹12,00,000); other currencies in thousands.
func money(minor float64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "−", -minor
	}
	units := int64(math.Round(minor))
	s := sign + symbol(currency) + groupDigits(strconv.FormatInt(units/100, 10), currency == "INR")
	if cents := units % 100; cents != 0 {
		s += "." + strconv.FormatInt(100+cents, 10)[1:]
	}
	return s
}

func symbol(currency string) string {
	if s, ok := symbols[currency]; ok {
		return s
	}
	return currency + " "
}

// groupDigits adds thousands separators: 1200000 → "1,200,000", or "12,00,000" the Indian way.
func groupDigits(digits string, indian bool) string {
	if len(digits) <= 3 {
		return digits
	}
	head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
	size := 3
	if indian {
		size = 2
	}
	var parts []string
	for len(head) > size {
		parts = append([]string{head[len(head)-size:]}, parts...)
		head = head[:len(head)-size]
	}
	return strings.Join(append(append([]string{head}, parts...), tail), ",")
}
