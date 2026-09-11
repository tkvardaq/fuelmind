package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type RollbackRecord struct {
	FailedVersion string    `json:"failed_version"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

func writeRollbackRecord(baseDir, failedVersion, reason string) error {
	rec := RollbackRecord{FailedVersion: failedVersion, Reason: reason, At: time.Now()}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(baseDir, "data", "rollback_occurred.json"), data, 0644)
}

func ReadRollbackRecord(baseDir string) (RollbackRecord, bool, error) {
	data, err := os.ReadFile(filepath.Join(baseDir, "data", "rollback_occurred.json"))
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

func DeleteRollbackRecord(baseDir string) error {
	return os.Remove(filepath.Join(baseDir, "data", "rollback_occurred.json"))
}