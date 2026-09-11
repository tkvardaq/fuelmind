package hardware

import (
	"runtime"
	"testing"
)

// TestDetectHardwareTier_AlwaysReturnsValidTier verifies the function
// always returns one of the four supported tier names regardless of
// the machine it runs on. The actual mapping is hardware-dependent
// and tested in TestDetectHardwareTier_Thresholds.
func TestDetectHardwareTier_AlwaysReturnsValidTier(t *testing.T) {
	tier := DetectHardwareTier()
	valid := map[string]bool{
		"basic": true, "standard": true, "enhanced": true, "pro": true,
	}
	if !valid[tier] {
		t.Errorf("DetectHardwareTier returned %q, want one of basic/standard/enhanced/pro", tier)
	}
}

// TestDetectHardwareTier_Thresholds verifies the CPU-core threshold
// mapping for the v1 simplified tier detection.
func TestDetectHardwareTier_Thresholds(t *testing.T) {
	cores := runtime.NumCPU()
	got := DetectHardwareTier()
	var want string
	switch {
	case cores < 4:
		want = "basic"
	case cores < 8:
		want = "standard"
	default:
		want = "enhanced"
	}
	if got != want {
		t.Errorf("DetectHardwareTier with %d cores = %q, want %q", cores, got, want)
	}
}
