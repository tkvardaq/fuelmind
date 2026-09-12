package hardware

import (
	"runtime"
	"testing"
)

func TestDetectHardwareTierReturnsAValidTier(t *testing.T) {
	tier := DetectHardwareTier()
	if !ValidTier(tier) {
		t.Errorf("DetectHardwareTier returned %q", tier)
	}
	t.Logf("this machine: %d cores, %.1f GiB RAM -> %s", runtime.NumCPU(), TotalRAMGB(), tier)
}

// Automatic detection never claims a GPU tier: enhanced/pro are opt-in
// via FUELMIND_HARDWARE_TIER, because a 7B model on a CPU-only shop PC
// would blow the 8s latency budget.
func TestTierTable(t *testing.T) {
	cases := []struct {
		cores int
		ram   float64
		want  string
	}{
		{2, 8, TierBasic},
		{4, 4, TierBasic},
		{4, 8, TierStandard},
		{8, 16, TierStandard},
		{16, 64, TierStandard},
		{8, 0, TierStandard}, // RAM unknown: decide on cores alone
	}
	for _, c := range cases {
		if got := tierFor(c.cores, c.ram); got != c.want {
			t.Errorf("tierFor(%d cores, %.0f GiB) = %q, want %q", c.cores, c.ram, got, c.want)
		}
	}
}

func TestValidTier(t *testing.T) {
	if ValidTier("turbo") || !ValidTier(TierPro) {
		t.Error("ValidTier is wrong")
	}
}
