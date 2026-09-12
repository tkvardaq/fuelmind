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
