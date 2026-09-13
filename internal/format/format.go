// Package format renders the figures FuelMind shows to the owner. The
// dashboard and the messages the station sends out share it, so a number
// reads the same whether the owner sees it on the page or on their phone.
package format

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Money renders a whole-rupee amount: "PKR 92,662".
func Money(n float64) string { return "PKR " + Num(n, 0) }

// Price keeps the paisa: a per-litre price of 232.50 must not be shown
// as 233.
func Price(n float64) string { return "PKR " + Num(n, 2) }

// Liters renders a volume: "340.00 L".
func Liters(n float64) string { return Num(n, 2) + " L" }

// Num renders n with thousands separators and a fixed number of decimals.
func Num(n float64, decimals int) string {
	pow := math.Pow10(decimals)
	rounded := math.Round(math.Abs(n) * pow)
	if rounded == 0 {
		n = 0 // avoid "-0.00"
	}
	whole := int64(rounded / pow)
	frac := int64(rounded) - whole*int64(pow)
	wstr := strconv.FormatInt(whole, 10)
	var b strings.Builder
	if n < 0 {
		b.WriteByte('-')
	}
	for i, c := range wstr {
		if i > 0 && (len(wstr)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if decimals > 0 {
		b.WriteByte('.')
		fstr := strconv.FormatInt(frac, 10)
		for len(fstr) < decimals {
			fstr = "0" + fstr
		}
		b.WriteString(fstr)
	}
	return b.String()
}

// Date renders "2026-09-07" as "Mon 7 Sep 2026". Anything that is not a
// date is returned unchanged.
func Date(t string) string {
	d, err := time.Parse("2006-01-02", t)
	if err != nil {
		return t
	}
	return d.Format("Mon 2 Jan 2006")
}
