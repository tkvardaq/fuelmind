package launcher

import (
	"os"
	"testing"
	"time"
)

func writeCorrupt(path string) error {
	return os.WriteFile(path, []byte("{not json"), 0o644)
}

// A probation window has to survive a launcher restart. If it does not,
// a reboot during probation silently commits a version that would
// otherwise have been rolled back — which is the whole case probation
// exists to cover.
func TestProbationSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(2 * time.Hour).Round(time.Second)

	if err := writeProbation(dir, "1.2.0", until, 1); err != nil {
		t.Fatalf("writeProbation: %v", err)
	}
	gotUntil, gotCrashes := readProbation(dir, "1.2.0")
	if !gotUntil.Equal(until) {
		t.Errorf("until = %v, want %v", gotUntil, until)
	}
	if gotCrashes != 1 {
		t.Errorf("crashes = %d, want 1", gotCrashes)
	}
}

func TestProbationIgnoresAnotherVersion(t *testing.T) {
	dir := t.TempDir()
	if err := writeProbation(dir, "1.2.0", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	// After a rollback the running version is a different one, and the
	// old window must not be applied to it.
	if until, _ := readProbation(dir, "1.1.0"); !until.IsZero() {
		t.Errorf("a window for 1.2.0 was applied to 1.1.0: %v", until)
	}
}

func TestProbationExpires(t *testing.T) {
	dir := t.TempDir()
	if err := writeProbation(dir, "1.2.0", time.Now().Add(-time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if until, _ := readProbation(dir, "1.2.0"); !until.IsZero() {
		t.Errorf("an expired window was still open: %v", until)
	}
	// An expired window is cleaned up rather than read again every boot.
	if until, _ := readProbation(dir, "1.2.0"); !until.IsZero() {
		t.Error("the expired window was not cleared")
	}
}

func TestProbationAbsentAndCorruptAreNotAnError(t *testing.T) {
	dir := t.TempDir()
	if until, crashes := readProbation(dir, "1.2.0"); !until.IsZero() || crashes != 0 {
		t.Errorf("no file should mean no window, got %v / %d", until, crashes)
	}
	// A half-written file (power cut) must not stop the launcher.
	if err := writeProbation(dir, "1.2.0", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := writeCorrupt(probationPath(dir)); err != nil {
		t.Fatal(err)
	}
	if until, _ := readProbation(dir, "1.2.0"); !until.IsZero() {
		t.Error("a corrupt file was read as an open window")
	}
}

func TestClearProbationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := clearProbation(dir); err != nil {
		t.Errorf("clearing when there is nothing to clear: %v", err)
	}
	if err := writeProbation(dir, "1.2.0", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := clearProbation(dir); err != nil {
		t.Errorf("clearProbation: %v", err)
	}
	if until, _ := readProbation(dir, "1.2.0"); !until.IsZero() {
		t.Error("the window was still there after being cleared")
	}
}
