package launcher

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	childOnce sync.Once
	childBin  string
	childErr  error
)

func buildChild(t *testing.T) string {
	t.Helper()
	childOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fmchild")
		if err != nil {
			childErr = err
			return
		}
		name := "child"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		childBin = filepath.Join(dir, name)
		out, err := exec.Command("go", "build", "-o", childBin, "./testdata/child").CombinedOutput()
		if err != nil {
			childErr = err
			t.Logf("%s", out)
		}
	})
	if childErr != nil {
		t.Fatalf("build fake core: %v", childErr)
	}
	return childBin
}

func install(t *testing.T, base, v string) {
	t.Helper()
	if err := installVersion(base, v, buildChild(t)); err != nil {
		t.Fatal(err)
	}
}

func runs(t *testing.T, logPath string) []string {
	b, _ := os.ReadFile(logPath)
	return strings.Fields(string(b))
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestRunWithoutAnythingInstalled(t *testing.T) {
	err := New(Options{BaseDir: t.TempDir(), Logger: quietLogger()}).Run(context.Background())
	if err != errNoCurrentVersion {
		t.Fatalf("got %v, want errNoCurrentVersion", err)
	}
}

func TestBootstrapFreshInstallAndUpgrade(t *testing.T) {
	base, installDir := t.TempDir(), t.TempDir()
	b, _ := os.ReadFile(buildChild(t))
	_ = os.WriteFile(filepath.Join(installDir, CoreExeName), b, 0o755)

	t.Setenv("CHILD_BOOT_VERSION", "1.0.0")
	if v, err := Bootstrap(base, installDir); err != nil || v != "1.0.0" {
		t.Fatalf("fresh bootstrap: v=%q err=%v", v, err)
	}
	if cur, _ := ReadCurrentVersion(base); cur != "1.0.0" {
		t.Fatalf("current = %q", cur)
	}
	// Same bundled version again: nothing changes.
	if v, _ := Bootstrap(base, installDir); v != "" {
		t.Errorf("repeat bootstrap adopted %q", v)
	}
	// MSI upgrade ships 1.1.0: becomes current, 1.0.0 becomes previous.
	t.Setenv("CHILD_BOOT_VERSION", "1.1.0")
	if v, err := Bootstrap(base, installDir); err != nil || v != "1.1.0" {
		t.Fatalf("upgrade bootstrap: v=%q err=%v", v, err)
	}
	cur, _ := ReadCurrentVersion(base)
	prev, _ := ReadPreviousVersion(base)
	if cur != "1.1.0" || prev != "1.0.0" {
		t.Errorf("after upgrade current=%q previous=%q", cur, prev)
	}
	// An older bundled core never downgrades an auto-updated install.
	t.Setenv("CHILD_BOOT_VERSION", "0.9.0")
	_, _ = Bootstrap(base, installDir)
	if cur, _ := ReadCurrentVersion(base); cur != "1.1.0" {
		t.Errorf("older bundled core downgraded current to %q", cur)
	}
}

func TestCrashLoopRollsBackAndRecords(t *testing.T) {
	base := t.TempDir()
	logPath := filepath.Join(base, "runs.txt")
	t.Setenv("CHILD_LOG", logPath)
	t.Setenv("CHILD_MODE_v2", "exit:1")
	t.Setenv("CHILD_MODE_v1", "sleep")
	install(t, base, "v1")
	install(t, base, "v2")
	_ = WriteCurrentVersion(base, "v2")
	_ = WritePreviousVersion(base, "v1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = New(Options{BaseDir: base, Logger: quietLogger()}).Run(ctx); close(done) }()
	ok := waitFor(t, 30*time.Second, func() bool {
		r := runs(t, logPath)
		return len(r) > 0 && r[len(r)-1] == "v1"
	})
	cancel()
	<-done
	if !ok {
		t.Fatalf("no rollback; runs=%v", runs(t, logPath))
	}
	crashes := strings.Count(strings.Join(runs(t, logPath), " "), "v2")
	if crashes != crashLoopMaxCrashes {
		t.Errorf("rolled back after %d crashes, want %d", crashes, crashLoopMaxCrashes)
	}
	rec, ok2, err := ReadRollbackRecord(base)
	if !ok2 || err != nil || rec.FailedVersion != "v2" {
		t.Errorf("rollback record: %+v ok=%v err=%v", rec, ok2, err)
	}
	if _, err := os.Stat(filepath.Join(base, "logs", "core.log")); err != nil {
		t.Errorf("core.log not created: %v", err)
	}
}

func TestCleanExitIsNotAHotLoop(t *testing.T) {
	base := t.TempDir()
	logPath := filepath.Join(base, "runs.txt")
	t.Setenv("CHILD_LOG", logPath)
	t.Setenv("CHILD_MODE_v1", "exit:0")
	install(t, base, "v1")
	_ = WriteCurrentVersion(base, "v1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = New(Options{BaseDir: base, Logger: quietLogger()}).Run(ctx)
	if n := len(runs(t, logPath)); n > 4 {
		t.Errorf("child relaunched %d times in 3s; want backoff", n)
	}
}

func TestHandoffHealthyUpdateCommits(t *testing.T) {
	base := t.TempDir()
	port := freePort(t)
	logPath := filepath.Join(base, "runs.txt")
	t.Setenv("CHILD_LOG", logPath)
	t.Setenv("CHILD_MODE_v1", "exit:42")
	t.Setenv("CHILD_MODE_v2", "serve")
	install(t, base, "v1")
	install(t, base, "v2")
	install(t, base, "v0") // stale version, pruned after probation
	_ = WriteCurrentVersion(base, "v1")
	_ = WritePendingVersion(base, "v2")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sup := New(Options{BaseDir: base, Port: port, Probation: 2 * time.Second, HealthTimeout: 20 * time.Second, Logger: quietLogger()})
	go func() { _ = sup.Run(ctx); close(done) }()
	pruned := waitFor(t, 30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(VersionsDir(base), "v0"))
		return os.IsNotExist(err)
	})
	cancel()
	<-done
	cur, _ := ReadCurrentVersion(base)
	prev, _ := ReadPreviousVersion(base)
	_, pending, _ := ReadPending(base)
	if cur != "v2" || prev != "v1" || pending {
		t.Errorf("current=%q previous=%q pendingLeft=%v", cur, prev, pending)
	}
	if !pruned {
		t.Error("stale version v0 not pruned after probation")
	}
}

func TestUnhealthyUpdateRollsBack(t *testing.T) {
	base := t.TempDir()
	logPath := filepath.Join(base, "runs.txt")
	t.Setenv("CHILD_LOG", logPath)
	t.Setenv("CHILD_MODE_v1", "exit:42")
	t.Setenv("CHILD_MODE_v2", "sleep") // runs, but never answers /healthz
	install(t, base, "v1")
	install(t, base, "v2")
	_ = WriteCurrentVersion(base, "v1")
	_ = WritePendingVersion(base, "v2")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sup := New(Options{BaseDir: base, Port: freePort(t), HealthTimeout: 2 * time.Second, Logger: quietLogger()})
	go func() { _ = sup.Run(ctx); close(done) }()
	ok := waitFor(t, 30*time.Second, func() bool {
		_, has, _ := ReadRollbackRecord(base)
		return has
	})
	cancel()
	<-done
	if !ok {
		t.Fatal("unhealthy update was not rolled back")
	}
	if cur, _ := ReadCurrentVersion(base); cur != "v1" {
		t.Errorf("current = %q, want v1", cur)
	}
	rec, _, _ := ReadRollbackRecord(base)
	if !strings.Contains(rec.Reason, "healthz") {
		t.Errorf("reason = %q", rec.Reason)
	}
}

func TestPointerRejectsTraversal(t *testing.T) {
	if err := WriteCurrentVersion(t.TempDir(), `..\..\x`); err == nil {
		t.Error("wrote a path-traversal version into current.json")
	}
}
