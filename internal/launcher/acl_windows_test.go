//go:build windows

package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// sddl returns the DACL of path in SDDL form, which names trustees by
// short code (LS local service, SY system, BA administrators,
// AU authenticated users, BU built-in users) regardless of locale.
func sddl(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	return sd.String()
}

func TestHardenDataDir(t *testing.T) {
	base := t.TempDir()
	drop := filepath.Join(base, posDropFolder)
	restoreACL(t, base)
	if err := os.MkdirAll(drop, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := HardenDataDir(base); err != nil {
		t.Fatalf("HardenDataDir: %v", err)
	}

	got := sddl(t, base)
	// "D:P" marks the DACL as protected: inherited entries are gone.
	if !strings.Contains(got, "D:P") {
		t.Errorf("data folder still inherits permissions: %s", got)
	}
	for _, trustee := range []string{";;;LS)", ";;;SY)", ";;;BA)"} {
		if !strings.Contains(got, trustee) {
			t.Errorf("data folder ACL missing %s: %s", trustee, got)
		}
	}
	for _, open := range []string{";;;BU)", ";;;AU)", ";;;WD)"} {
		if strings.Contains(got, open) {
			t.Errorf("data folder is still open to ordinary users (%s): %s", open, got)
		}
	}

	dropACL := sddl(t, drop)
	if !strings.Contains(dropACL, "D:P") {
		t.Errorf("pos_drop still inherits permissions: %s", dropACL)
	}
	if !strings.Contains(dropACL, ";;;AU)") {
		t.Errorf("pos_drop must stay writable for the POS export account: %s", dropACL)
	}

	// An ordinary (unelevated) account must no longer be able to read
	// or write the station data. The test process is exactly that.
	if err := os.WriteFile(filepath.Join(base, "probe.txt"), []byte("x"), 0o644); err == nil {
		t.Error("an unelevated account can still write to the data folder")
	}
	// pos_drop stays usable, which is how the POS delivers exports.
	if err := os.WriteFile(filepath.Join(drop, "export.csv"), []byte("x"), 0o644); err != nil {
		t.Errorf("writing to pos_drop failed: %v", err)
	}
}

// restoreACL hands the directory back to the test user so t.TempDir can
// delete it (the owner may always rewrite the DACL).
func restoreACL(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() {
		_ = exec.Command("icacls", path, "/reset", "/T", "/C", "/Q").Run()
	})
}
