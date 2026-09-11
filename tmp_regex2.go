package main

import (
	"fmt"
	"regexp"
)

func main() {
	q := "how much did we make today?"
	// Try matching make or made
	r := regexp.MustCompile(`(?i)(?:\b(make|made)\b.*\b(today|now)\b)|(?:\b(today|now)\b.*\b(make|made|sales|revenue|sold|earning)\b)`)
	fmt.Println("matches:", r.MatchString(q))
}