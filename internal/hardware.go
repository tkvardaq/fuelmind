package hardware

import "runtime"

// detectHardwareTier returns the hardware tier based on CPU core count.
// This is a v1 simplification — RAM tier thresholds can be overridden
// via config or command-line flags in a future release.
func detectHardwareTier() string {
	cores := runtime.NumCPU()
	switch {
	case cores < 4:
		return "basic"
	case cores < 8:
		return "standard"
	default:
		return "enhanced"
	}
}
