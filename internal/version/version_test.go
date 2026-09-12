package version

import (
	"fmt"
	"testing"
)

func TestValid(t *testing.T) {
	for _, v := range []string{"1.0.0", "1.0.0-pilot", "0.2.0+build.5", "dev"} {
		if !Valid(v) {
			t.Errorf("Valid(%q) = false", v)
		}
	}
	for _, v := range []string{"", "..", `..\..\escaped`, "../x", "1.0/2", "a b", ".hidden", "1..2"} {
		if Valid(v) {
			t.Errorf("Valid(%q) = true", v)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.10.0", "1.9.0", 1},
		{"1.0.0", "1.0.0-pilot", 1},
		{"1.0.0-pilot", "1.0.0", -1},
		{"v2.0", "1.9.9", 1},
		{"1.0", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRolloutIsUnbiased(t *testing.T) {
	in := 0
	const n = 20000
	for i := 0; i < n; i++ {
		if InRollout(fmt.Sprintf("FM-%07d", i), 10) {
			in++
		}
	}
	pct := float64(in) * 100 / n
	if pct < 9 || pct > 11 {
		t.Errorf("rollout_pct=10 selects %.1f%% of stations", pct)
	}
}
