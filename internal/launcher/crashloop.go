package launcher

import "time"

// CrashLoopDetector tracks non-graceful exit timestamps and flags a crash
// loop when maxCrashes happen within window.
type CrashLoopDetector struct {
	maxCrashes int
	window     time.Duration
	timestamps []time.Time
}

// NewCrashLoopDetector builds a detector.
func NewCrashLoopDetector(maxCrashes int, window time.Duration) *CrashLoopDetector {
	return &CrashLoopDetector{maxCrashes: maxCrashes, window: window}
}

// RecordCrash records a non-graceful exit and returns true when this
// crash is the maxCrashes-th inside the window.
func (d *CrashLoopDetector) RecordCrash(at time.Time) bool {
	d.timestamps = append(d.timestamps, at)
	cutoff := at.Add(-d.window)
	kept := d.timestamps[:0]
	for _, t := range d.timestamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	d.timestamps = kept
	return len(d.timestamps) >= d.maxCrashes
}

// Reset forgets all recorded crashes.
func (d *CrashLoopDetector) Reset() {
	d.timestamps = nil
}
