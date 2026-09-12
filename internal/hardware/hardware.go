// Package hardware detects the LLM sizing tier for a station's PC.
//
// v1 rule (plan Phase 7): the tier is set by CPU cores and installed RAM.
// Enhanced and Pro assume a GPU, which we cannot detect reliably without
// extra dependencies, so automatic detection stops at Standard; support
// sets FUELMIND_HARDWARE_TIER=enhanced|pro on machines that have one.
package hardware

import (
	"runtime"
)

// Tiers.
const (
	TierBasic    = "basic"
	TierStandard = "standard"
	TierEnhanced = "enhanced"
	TierPro      = "pro"
)

// DetectHardwareTier returns the tier for this machine.
func DetectHardwareTier() string {
	return tierFor(runtime.NumCPU(), TotalRAMGB())
}

// tierFor is the pure decision table, unit-tested independently of the
// machine the tests run on.
func tierFor(cores int, ramGB float64) string {
	switch {
	case cores < 4 || (ramGB > 0 && ramGB < 7.5):
		return TierBasic
	default:
		return TierStandard
	}
}

// TotalRAMGB returns installed memory in GiB, or 0 when unknown.
func TotalRAMGB() float64 { return totalRAMBytes() / (1024 * 1024 * 1024) }

// ValidTier reports whether s is a known tier name.
func ValidTier(s string) bool {
	switch s {
	case TierBasic, TierStandard, TierEnhanced, TierPro:
		return true
	}
	return false
}
