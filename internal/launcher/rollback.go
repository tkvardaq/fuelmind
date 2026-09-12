package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// RollbackRecord is written by the launcher when it rolls back a version
// and read by the core, which forwards it to the cloud in the next
// heartbeat.
type RollbackRecord struct {
	FailedVersion string    `json:"failed_version"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

func rollbackRecordPath(baseDir string) string {
	return filepath.Join(baseDir, "data", "rollback_occurred.json")
}

func writeRollbackRecord(baseDir, failedVersion, reason string) error {
	rec := RollbackRecord{FailedVersion: failedVersion, Reason: reason, At: time.Now()}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	p := rollbackRecordPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// ReadRollbackRecord returns the pending rollback record, if any.
func ReadRollbackRecord(baseDir string) (RollbackRecord, bool, error) {
	data, err := os.ReadFile(rollbackRecordPath(baseDir))
	if os.IsNotExist(err) {
		return RollbackRecord{}, false, nil
	}
	if err != nil {
		return RollbackRecord{}, false, err
	}
	var rec RollbackRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return RollbackRecord{}, false, err
	}
	return rec, true, nil
}

// DeleteRollbackRecord removes the record once the core has taken it over.
func DeleteRollbackRecord(baseDir string) error {
	err := os.Remove(rollbackRecordPath(baseDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// failedVersionsPath holds the versions that were rolled back on this
// station. The core's update agent refuses to install them again, so a
// bad release cannot flap: install, crash, roll back, re-download.
func failedVersionsPath(baseDir string) string {
	return filepath.Join(baseDir, "data", "failed_versions.json")
}

// FailedVersion is one quarantined version.
type FailedVersion struct {
	Version string    `json:"version"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// ReadFailedVersions returns the quarantined versions for this station.
func ReadFailedVersions(baseDir string) ([]FailedVersion, error) {
	data, err := os.ReadFile(failedVersionsPath(baseDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []FailedVersion
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// IsVersionQuarantined reports whether v previously failed here.
func IsVersionQuarantined(baseDir, v string) bool {
	list, err := ReadFailedVersions(baseDir)
	if err != nil {
		return false
	}
	for _, f := range list {
		if f.Version == v {
			return true
		}
	}
	return false
}

// quarantineVersion records that v failed on this station. Support can
// clear the quarantine by deleting data\failed_versions.json.
func quarantineVersion(baseDir, v, reason string) error {
	list, err := ReadFailedVersions(baseDir)
	if err != nil {
		return err
	}
	for _, f := range list {
		if f.Version == v {
			return nil
		}
	}
	list = append(list, FailedVersion{Version: v, Reason: reason, At: time.Now()})
	if len(list) > 20 {
		list = list[len(list)-20:]
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	p := failedVersionsPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// QuarantineForTest records a failed version. Exported for tests in other
// packages that simulate a rollback.
func QuarantineForTest(baseDir, v, reason string) error { return quarantineVersion(baseDir, v, reason) }
