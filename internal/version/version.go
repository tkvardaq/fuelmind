// Package version holds the version helpers shared by the core's update
// agent, the launcher and the control plane: validation (versions become
// directory names), ordering, and deterministic staged-rollout buckets.
package version

import (
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"
)

var validRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+\-]{0,63}$`)

// Valid reports whether v is safe to use as a version string and as a
// directory name: letters, digits, '.', '-', '+', no "..", max 64 chars.
func Valid(v string) bool {
	return validRe.MatchString(v) && !strings.Contains(v, "..")
}

// Compare orders two versions like semver: numeric dot-separated core,
// then a release sorts after any pre-release of the same core
// ("1.0.0" > "1.0.0-pilot"). Non-numeric parts compare as strings.
// Returns -1, 0 or 1.
func Compare(a, b string) int {
	a, b = strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")
	a, _, _ = strings.Cut(a, "+")
	b, _, _ = strings.Cut(b, "+")
	ac, apre, _ := strings.Cut(a, "-")
	bc, bpre, _ := strings.Cut(b, "-")
	ap, bp := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var x, y string
		if i < len(ap) {
			x = ap[i]
		}
		if i < len(bp) {
			y = bp[i]
		}
		if c := comparePart(x, y); c != 0 {
			return c
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	return comparePart(apre, bpre)
}

func comparePart(x, y string) int {
	xi, xerr := strconv.Atoi(orZero(x))
	yi, yerr := strconv.Atoi(orZero(y))
	if xerr == nil && yerr == nil {
		switch {
		case xi < yi:
			return -1
		case xi > yi:
			return 1
		}
		return 0
	}
	return strings.Compare(x, y)
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// RolloutBucket maps a station to a stable bucket in [0, 100).
func RolloutBucket(stationID string) int {
	h := sha256.Sum256([]byte(stationID))
	return int(binary.BigEndian.Uint32(h[:4]) % 100)
}

// InRollout reports whether a station falls inside a pct% staged rollout.
func InRollout(stationID string, pct int) bool {
	if pct >= 100 {
		return true
	}
	return RolloutBucket(stationID) < pct
}
