//go:build !windows

package launcher

// HardenDataDir is a no-op outside Windows.
func HardenDataDir(string) error { return nil }
