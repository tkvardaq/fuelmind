package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
)

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
		return "", err
	}
	return p.Version, nil
}

// writePointerAtomic writes via a temp file + os.Rename in the SAME
// directory as the target, which is atomic on NTFS. Never write
// directly to the target path.
func writePointerAtomic(path, version string) error {
	data, err := json.Marshal(VersionPointer{Version: version})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic, same volume, same dir
}

func ReadCurrentVersion(baseDir string) (string, error) {
	return readPointer(filepath.Join(baseDir, "current.json"))
}

func ReadPreviousVersion(baseDir string) (string, error) {
	return readPointer(filepath.Join(baseDir, "previous.json"))
}

func WriteCurrentVersion(baseDir, version string) error {
	return writePointerAtomic(filepath.Join(baseDir, "current.json"), version)
}

func WritePreviousVersion(baseDir, version string) error {
	return writePointerAtomic(filepath.Join(baseDir, "previous.json"), version)
}

func PendingPath(baseDir string) string {
	return filepath.Join(baseDir, "pending.json")
}

func ReadPending(baseDir string) (string, bool, error) {
	v, err := readPointer(PendingPath(baseDir))
	if err != nil {
		return "", false, err
	}
	return v, v != "", nil
}

func DeletePending(baseDir string) error {
	err := os.Remove(PendingPath(baseDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func CoreBinaryPath(baseDir, version string) string {
	return filepath.Join(baseDir, "versions", version, "FuelMindCore.exe")
}