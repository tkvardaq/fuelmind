package launcher

import "time"

// CrashLoopDetector tracks non-graceful exit timestamps in a ring
// buffer and flags a crash loop when too many happen within a window.
type CrashLoopDetector struct {
	maxCrashes int
	window     time.Duration
	timestamps []time.Time
}

func NewCrashLoopDetector(maxCrashes int, window time.Duration) *CrashLoopDetector {
	return &CrashLoopDetector{maxCrashes: maxCrashes, window: window}
}

// RecordCrash records a non-graceful exit and returns true if this
// crash means a crash loop should now be declared.
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
	return len(d.timestamps) > d.maxCrashes
}

func (d *CrashLoopDetector) Reset() {
	d.timestamps = nil
}