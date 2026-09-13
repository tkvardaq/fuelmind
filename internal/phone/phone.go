// Package phone canonicalizes customer phone numbers. It is a leaf
// package so both the normalizer (which cleans POS rows on the way in)
// and storage (which matches repayments to a customer) can agree on what
// counts as the same person.
package phone

import "strings"

// DefaultCountryCode is the dialling code assumed for national-format
// numbers (a leading 0). Pakistan, where FuelMind's first stations are.
const DefaultCountryCode = "92"

// Normalize puts a customer phone number into one canonical form so
// the same person is one credit account no matter how the POS wrote them
// down. Station staff type numbers however they like, and a POS export
// happily carries all of these for one customer:
//
//	+92-300-1234567   0300 1234567   03001234567   0092 300 1234567
//
// All of them become "+923001234567". Without this each spelling is its
// own row in credit_outstanding, so one customer's balance is split
// across several accounts and no credit limit means anything.
//
// The number is returned unchanged (trimmed) when it cannot be read as a
// phone number at all — an account reference like "LOYALTY-88", say. It
// is better to keep the station's own identifier than to mangle it.
func Normalize(raw string) string {
	return NormalizeCC(raw, DefaultCountryCode)
}

// NormalizeCC is Normalize with an explicit country code, for
// stations outside Pakistan. An empty cc leaves national numbers alone.
func NormalizeCC(raw, cc string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	// Keep only digits. Anything else in the string (spaces, dashes,
	// brackets, a leading +) is formatting, not identity.
	var digits strings.Builder
	letters := false
	for _, r := range trimmed {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters = true
		}
	}
	d := digits.String()

	// A value with letters in it is an account reference, not a phone
	// number; so is something too short to dial. Leave both as they are.
	if letters || len(d) < 7 {
		return trimmed
	}

	switch {
	case strings.HasPrefix(d, "00"):
		// International prefix dialled the old way: 0092... -> 92...
		d = strings.TrimPrefix(d, "00")
	case strings.HasPrefix(d, "0"):
		// National format: 0300... -> <cc>300...
		if cc == "" {
			return "+" + d
		}
		d = cc + strings.TrimPrefix(d, "0")
	}
	return "+" + d
}
