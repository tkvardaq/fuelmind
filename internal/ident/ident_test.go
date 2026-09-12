package ident

import (
	"strings"
	"testing"
)

func TestGenerateStationIDShape(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s := id.StationID.String()
	if !IsValidStationID(s) {
		t.Errorf("generated station_id %q failed IsValidStationID", s)
	}
	if !strings.HasPrefix(s, StationIDPrefix) {
		t.Errorf("missing prefix %q: %q", StationIDPrefix, s)
	}
	if len(s) != len(StationIDPrefix)+StationIDSuffixLen {
		t.Errorf("wrong length %d, want %d", len(s), len(StationIDPrefix)+StationIDSuffixLen)
	}
}

func TestGenerateAPIKeyShape(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s := id.APIKey.String()
	if len(s) < 40 {
		t.Errorf("api key too short: %d", len(s))
	}
	// Must be URL-safe (no '+' '/' '=' which are base64 std).
	for _, c := range s {
		if c == '+' || c == '/' || c == '=' {
			t.Errorf("api key contains non-URL-safe char %q in %q", c, s)
		}
	}
}

// TestStationIDCollisionFreeAt10k verifies that 10,000 consecutive
// generations produce no duplicates. The station_id alphabet has
// StationIDSuffixLen characters, so the suffix space is alphabet^len.
//
// For 32 chars × 7 positions = 32^7 ≈ 34B IDs; birthday collision
// probability for 10k pairs is ~10k² / (2 × 34B) ≈ 0.0015% —
// effectively zero. If this test fails, the alphabet or length
// changed in a way that broke the collision budget.
func TestStationIDCollisionFreeAt10k(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		id, err := GenerateIdentity()
		if err != nil {
			t.Fatal(err)
		}
		s := id.StationID.String()
		if seen[s] {
			t.Fatalf("duplicate station_id %q at iteration %d", s, i)
		}
		seen[s] = true
	}
}

func TestIsValidStationID(t *testing.T) {
	good := []string{
		// 7-char suffix (current production length)
		"FM-2345678",
		"FM-ABCDEFG",
		"FM-ZZZZZZZ",
		"FM-9K7QP23",
	}
	for _, s := range good {
		if !IsValidStationID(s) {
			t.Errorf("expected valid: %q", s)
		}
	}
	bad := []string{
		"",             // empty
		"FM-",          // missing suffix
		"FM-2345",      // too short (4)
		"FM-234567",    // too short (6, but production is 7)
		"FM-23456789",  // too long (8)
		"fm-2345678",   // wrong prefix case
		"FM-1234567",   // has 1 (excluded)
		"FM-IO234567",  // has I (excluded)
		"FM-OL234567",  // has O (excluded)
		"FM-LL234567",  // has L (excluded)
		"FM-2345!!!",   // punctuation
		"FM-234 678",   // space
		"FM-2345678\n", // trailing newline
		"XX-2345678",   // wrong prefix
	}
	for _, s := range bad {
		if IsValidStationID(s) {
			t.Errorf("expected invalid: %q", s)
		}
	}
}

func TestMaskAPIKey(t *testing.T) {
	if got := MaskAPIKey("abcd"); got != "***" {
		t.Errorf("short key: got %q want ***", got)
	}
	if got := MaskAPIKey("abcdefghwxyz"); got != "abcd…wxyz" {
		t.Errorf("normal key: got %q want abcd…wxyz", got)
	}
}

func TestGenerateIdentityDistinct(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	if a.StationID == b.StationID {
		t.Errorf("two consecutive generations produced the same station_id: %q", a.StationID)
	}
	if a.APIKey == b.APIKey {
		t.Errorf("two consecutive generations produced the same api_key")
	}
}
