package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// probationFile holds the post-update probation window across a restart
// of the launcher itself.
const probationFile = "probation.json"

// probationState is what survives a launcher restart. Without it, a
// service restart during probation — a reboot, a `sc stop`, the launcher
// crashing — silently ends the window, and a version that would have
// been rolled back runs on indefinitely instead. That is exactly the
// case probation exists for.
type probationState struct {
	Version string    `json:"version"`
	Until   time.Time `json:"until"`
	Crashes int       `json:"crashes"`
}

func probationPath(baseDir string) string { return filepath.Join(baseDir, probationFile) }

// readProbation returns the saved window, if one is still open for this
// version. A window for a different version, or one that has already
// passed, is treated as absent — and cleaned up.
func readProbation(baseDir, current string) (until time.Time, crashes int) {
	data, err := os.ReadFile(probationPath(baseDir))
	if err != nil {
		return time.Time{}, 0
	}
	var st probationState
	if err := json.Unmarshal(data, &st); err != nil {
		_ = clearProbation(baseDir)
		return time.Time{}, 0
	}
	if st.Version != current || !time.Now().Before(st.Until) {
		_ = clearProbation(baseDir)
		return time.Time{}, 0
	}
	return st.Until, st.Crashes
}

// writeProbation records the open window. It is written via a temp file
// and a rename, which is atomic on NTFS, so a power cut cannot leave a
// half-written file behind.
func writeProbation(baseDir, version string, until time.Time, crashes int) error {
	data, err := json.Marshal(probationState{Version: version, Until: until, Crashes: crashes})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	tmp := probationPath(baseDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, probationPath(baseDir))
}

// clearProbation ends the window.
func clearProbation(baseDir string) error {
	err := os.Remove(probationPath(baseDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
