//go:build !windows

package hardware

import (
	"os"
	"strconv"
	"strings"
)

// totalRAMBytes reads MemTotal from /proc/meminfo; 0 when unavailable.
func totalRAMBytes() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
