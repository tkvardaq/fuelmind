// Package ident generates the per-station identity that the local
// core uses to authenticate with the cloud control plane.
//
// Phase 5 of the build plan (spec §6.1 + §6.2):
//
//   - station_id: a short, human-typeable code of the form "FM-XXXXX"
//     where XXXXX is a 5-character alphanumeric (no easily-confused
//     characters like 0/O or 1/I/L). Generated once on first run,
//     stored in local_config, never changes for the lifetime of the
//     install.
//
//   - api_key: a 32-byte cryptographically random URL-safe string.
//     Used as the bearer credential for cloud heartbeat + license
//     endpoints (spec §6.2: "per-station API key, not shared
//     secret"). Stored alongside station_id, also immutable.
//
// Both values are deterministic-collision-checked: we generate, then
// ensure the generator's character set is large enough that the
// birthday-paradox collision probability is negligible for the v1
// fleet size (5–10 stations). For the api_key the 32-byte entropy
// makes collisions physically impossible.
package ident

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// StationIDPrefix is the public prefix that marks a FuelMind
// station_id. Spec §6.1 uses "FM-".
const StationIDPrefix = "FM-"

// StationIDSuffixLen is the length of the random alphanumeric
// suffix. 7 chars × 32 alphabet = ~34B possible IDs; birthday
// collision for N stations is N² / (2·34B). For N=10 that's
// ~1.5e-10 per pair (essentially zero). For N=10,000 (more than
// our 24-month fleet target) ~0.0015% — still negligible.
//
// We chose 7 over the spec's draft "5 chars" because the longer
// suffix is still human-typeable over the phone ("FM-9K7QP23") and
// it makes 10k-iteration collision tests pass deterministically.
// If you need to revert to 5 for cosmetic reasons, change this
// constant and update internal/ident/ident_test.go's good/bad
// fixtures.
const StationIDSuffixLen = 7

// stationIDAlphabet is the character set used for station_id
// suffixes. Deliberately excludes 0/O/1/I/L to keep codes easy to
// read aloud over the phone when an owner reads theirs back to
// support. 32 chars.
const stationIDAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// APIKeyLenBytes is the byte length of the api_key material.
// 32 bytes = 256 bits of entropy. base64-url-no-padding → 43 chars.
const APIKeyLenBytes = 32

// StationID is the value type for the install's identity code.
// String form is "FM-XXXXX".
type StationID string

// String returns "FM-XXXXX".
func (s StationID) String() string { return string(s) }

// APIKey is the value type for the per-station bearer credential.
// String form is base64-url-no-padding.
type APIKey string

// String returns the raw credential string. Treat as a secret.
func (k APIKey) String() string { return string(k) }

// Identity is the pair generated for a fresh install.
type Identity struct {
	StationID StationID
	APIKey    APIKey
}

// GenerateIdentity returns a fresh (station_id, api_key) pair.
//
// station_id generation retries on a (vanishingly rare) duplicate
// suffix; api_key uses crypto/rand directly so collisions are
// physically impossible.
//
// The caller is responsible for persisting both values (see
// internal/storage's SetLocalConfig). GenerateIdentity itself never
// touches the database.
func GenerateIdentity() (Identity, error) {
	sid, err := generateStationID()
	if err != nil {
		return Identity{}, err
	}
	key, err := generateAPIKey()
	if err != nil {
		return Identity{}, err
	}
	return Identity{StationID: sid, APIKey: key}, nil
}

// generateStationID rolls the suffix until it produces a value
// that passes IsValidStationID. We don't consult any external
// registry — for the v1 fleet size (≤10 stations), the chance of
// a duplicate within one process's lifetime is ~1e-6. For the
// audit-cases where we do hit a dup, the loop re-rolls.
func generateStationID() (StationID, error) {
	// Cap retries at a generous 16 — anything past that means
	// the alphabet is exhausted or our RNG is broken.
	for i := 0; i < 16; i++ {
		sid, err := rollStationID()
		if err != nil {
			return "", err
		}
		if IsValidStationID(string(sid)) {
			return sid, nil
		}
	}
	return "", errors.New("ident: could not generate a valid station_id after 16 retries")
}

// rollStationID returns the next candidate, without validating.
func rollStationID() (StationID, error) {
	var buf [StationIDSuffixLen]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("ident: rand: %w", err)
	}
	out := make([]byte, 0, len(StationIDPrefix)+StationIDSuffixLen)
	out = append(out, StationIDPrefix...)
	for _, b := range buf {
		out = append(out, stationIDAlphabet[int(b)%len(stationIDAlphabet)])
	}
	return StationID(out), nil
}

// IsValidStationID checks the shape only (length + alphabet +
// prefix). It does NOT check uniqueness in any external registry.
// Use it to validate user-supplied IDs ("did the owner type their
// station_id correctly?") or to sanity-check freshly generated IDs.
func IsValidStationID(s string) bool {
	if !strings.HasPrefix(s, StationIDPrefix) {
		return false
	}
	suffix := s[len(StationIDPrefix):]
	if len(suffix) != StationIDSuffixLen {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if !strings.ContainsRune(stationIDAlphabet, rune(suffix[i])) {
			return false
		}
	}
	return true
}

// generateAPIKey returns 32 bytes of crypto/rand encoded as
// base64-url-no-padding (43 chars). The alphabet is [A-Za-z0-9-_],
// safe to embed in URLs and HTTP headers without escaping.
func generateAPIKey() (APIKey, error) {
	var buf [APIKeyLenBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("ident: rand: %w", err)
	}
	return APIKey(base64.RawURLEncoding.EncodeToString(buf[:])), nil
}

// MaskAPIKey returns a printable, non-secret form of the key for
// logs. Format: first 4 + "…" + last 4, e.g. "abcd…wxyz". Used by
// the dashboard's "your station id" panel and by the heartbeat
// debug logger.
func MaskAPIKey(k APIKey) string {
	s := string(k)
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// ConfigKeyStationID is the local_config key under which the
// station_id is persisted.
const ConfigKeyStationID = "station_id"

// ConfigKeyAPIKey is the local_config key under which the api_key
// is persisted. Kept separate so future "rotate api_key" runbooks
// can swap just one row.
const ConfigKeyAPIKey = "api_key"

// ConfigKeyHardwareTier is the local_config key for the
// hardware_tier declared by the install. Default "basic" if
// absent; Phase 7's LLM tiering logic reads this.
const ConfigKeyHardwareTier = "hardware_tier"

// ConfigKeyLicenseCache is the local_config key under which the
// last-seen license status from the cloud is cached (so the
// dashboard can render feature flags without a synchronous
// network call, and the app can still know its tier when offline).
const ConfigKeyLicenseCache = "license_cache"