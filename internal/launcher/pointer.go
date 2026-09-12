package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fuelmind/fuelmind/internal/version"
)

// VersionPointer is the JSON body of current.json / previous.json /
// pending.json.
type VersionPointer struct {
	Version string `json:"version"`
}

func readPointer(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var p VersionPointer
	if err := json.Unmarshal(data, &p); err != nil {
		return "", fmt.Errorf("launcher: %s: %w", filepath.Base(path), err)
	}
	if p.Version != "" && !version.Valid(p.Version) {
		return "", fmt.Errorf("launcher: %s holds an invalid version %q", filepath.Base(path), p.Version)
	}
	return p.Version, nil
}

// writePointerAtomic writes via a temp file + rename in the same
// directory, which is atomic on NTFS.
func writePointerAtomic(path, v string) error {
	if !version.Valid(v) {
		return fmt.Errorf("launcher: refusing to write invalid version %q", v)
	}
	data, err := json.Marshal(VersionPointer{Version: v})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadCurrentVersion returns the version the launcher runs.
func ReadCurrentVersion(baseDir string) (string, error) {
	return readPointer(filepath.Join(baseDir, "current.json"))
}

// ReadPreviousVersion returns the version to roll back to.
func ReadPreviousVersion(baseDir string) (string, error) {
	return readPointer(filepath.Join(baseDir, "previous.json"))
}

// WriteCurrentVersion sets the version the launcher runs.
func WriteCurrentVersion(baseDir, v string) error {
	return writePointerAtomic(filepath.Join(baseDir, "current.json"), v)
}

// WritePreviousVersion sets the rollback target.
func WritePreviousVersion(baseDir, v string) error {
	return writePointerAtomic(filepath.Join(baseDir, "previous.json"), v)
}

// WritePendingVersion marks a staged update for the next handoff.
func WritePendingVersion(baseDir, v string) error {
	return writePointerAtomic(PendingPath(baseDir), v)
}

// PendingPath is the path of pending.json.
func PendingPath(baseDir string) string {
	return filepath.Join(baseDir, "pending.json")
}

// ReadPending returns the staged version, if any.
func ReadPending(baseDir string) (string, bool, error) {
	v, err := readPointer(PendingPath(baseDir))
	if err != nil {
		return "", false, err
	}
	return v, v != "", nil
}

// DeletePending clears pending.json.
func DeletePending(baseDir string) error {
	err := os.Remove(PendingPath(baseDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// VersionsDir is where every installed core version lives.
func VersionsDir(baseDir string) string { return filepath.Join(baseDir, "versions") }

// CoreBinaryPath is the core executable for a version.
func CoreBinaryPath(baseDir, v string) string {
	return filepath.Join(VersionsDir(baseDir), v, CoreExeName)
}

// CoreExeName is the core executable's file name in every version dir and
// next to the launcher in the install directory.
const CoreExeName = "FuelMindCore.exe"
