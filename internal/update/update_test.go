package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/launcher"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// makeArtifact writes a zip holding FuelMindCore.exe and returns its path
// and SHA-256.
func makeArtifact(t *testing.T, dir, body string) (string, string) {
	t.Helper()
	p := filepath.Join(dir, "artifact.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(launcher.CoreExeName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	sum, err := fileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, sum
}

func TestStageExtractsTheZip(t *testing.T) {
	base := t.TempDir()
	art, _ := makeArtifact(t, t.TempDir(), "new core binary")
	if err := Stage(base, "1.1.0", art); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(launcher.CoreBinaryPath(base, "1.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new core binary" {
		t.Errorf("staged binary = %q (the zip must be extracted, not copied)", got)
	}
	if v, ok, _ := launcher.ReadPending(base); !ok || v != "1.1.0" {
		t.Errorf("pending = %q ok=%v", v, ok)
	}
	if err := Stage(base, "1.1.0", art); err != nil {
		t.Errorf("re-staging the same version failed: %v", err)
	}
}

func TestStageRejectsBadVersionsAndCurrent(t *testing.T) {
	base := t.TempDir()
	art, _ := makeArtifact(t, t.TempDir(), "x")
	for _, v := range []string{"../../escaped", "..", "a/b"} {
		if err := Stage(base, v, art); err == nil {
			t.Errorf("Stage accepted version %q", v)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(base))
	for _, e := range entries {
		if e.Name() == "escaped" {
			t.Fatal("staging escaped the versions directory")
		}
	}
	_ = launcher.WriteCurrentVersion(base, "1.0.0")
	if err := Stage(base, "1.0.0", art); err == nil {
		t.Error("Stage re-installed the running version")
	}
}

func TestStageRejectsZipSlipAndMissingCore(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "evil.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../../evil.txt")
	_, _ = w.Write([]byte("pwned"))
	_ = zw.Close()
	_ = f.Close()
	if err := Stage(t.TempDir(), "1.2.0", p); err == nil {
		t.Error("Stage accepted a zip with a path-traversal entry")
	}

	p2 := filepath.Join(dir, "nocore.zip")
	f2, _ := os.Create(p2)
	zw2 := zip.NewWriter(f2)
	w2, _ := zw2.Create("readme.txt")
	_, _ = w2.Write([]byte("no exe here"))
	_ = zw2.Close()
	_ = f2.Close()
	if err := Stage(t.TempDir(), "1.2.0", p2); err == nil {
		t.Error("Stage accepted an artifact without FuelMindCore.exe")
	}
}

func TestDownloadResumes(t *testing.T) {
	body := strings.Repeat("abcdefghij", 500) // 5000 bytes
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.ServeContent(w, r, "artifact.zip", time.Now(), strings.NewReader(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "artifact.zip")
	if err := os.WriteFile(dst+".partial", []byte(body[:2000]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), srv.Client(), srv.URL+"/artifact.zip", dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("resumed file is wrong: %d bytes, want %d", len(got), len(body))
	}
	if requests != 1 {
		t.Errorf("%d requests for one resume, want 1", requests)
	}
}

// A server that ignores Range headers must still produce a correct file
// (this used to fail on Windows and grow the .partial file every cycle).
func TestDownloadWhenServerIgnoresRange(t *testing.T) {
	body := strings.Repeat("z", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dir := t.TempDir()
	dst := filepath.Join(dir, "a.zip")
	if err := os.WriteFile(dst+".partial", []byte("stale bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), srv.Client(), srv.URL, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != body {
		t.Errorf("got %d bytes, want %d", len(got), len(body))
	}
	if _, err := os.Stat(dst + ".partial"); !os.IsNotExist(err) {
		t.Error("partial file left behind")
	}
}

// --- end to end against a stub cloud ---

func newTestAgent(t *testing.T, base, cloudURL string, window Window, restart func()) *Agent {
	t.Helper()
	id, _ := ident.GenerateIdentity()
	return New(Config{
		Logger: quiet(), Identity: id, Version: "1.0.0", CloudURL: cloudURL,
		BaseDir: base, Supervised: true, Window: window, RequestRestart: restart,
		HardwareTier: "standard",
	})
}

func stubCloud(t *testing.T, rel *Release, artifact string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/updates/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("update check sent without an Authorization header")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if rel == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/artifacts/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, artifact)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTickStagesAndHandsOff(t *testing.T) {
	base := t.TempDir()
	art, sum := makeArtifact(t, t.TempDir(), "core 1.1.0")
	rel := &Release{Version: "1.1.0", ArtifactURL: "/artifacts/a.zip", ChecksumSHA256: strings.ToUpper(sum), RolloutPct: 100}
	srv := stubCloud(t, rel, art)

	var restarted atomic.Bool
	a := newTestAgent(t, base, srv.URL, AnyTime, func() { restarted.Store(true) })
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launcher.CoreBinaryPath(base, "1.1.0")); err != nil {
		t.Fatalf("update not staged: %v", err)
	}
	if !restarted.Load() {
		t.Error("no handoff requested inside the window")
	}
}

func TestTickWaitsForTheMaintenanceWindow(t *testing.T) {
	base := t.TempDir()
	art, sum := makeArtifact(t, t.TempDir(), "core 1.1.0")
	srv := stubCloud(t, &Release{Version: "1.1.0", ArtifactURL: "/artifacts/a.zip", ChecksumSHA256: sum, RolloutPct: 100}, art)
	var restarted atomic.Bool
	never := func(time.Time) bool { return false }
	a := newTestAgent(t, base, srv.URL, never, func() { restarted.Store(true) })
	staged, err := a.Tick(context.Background())
	if err != nil || !staged {
		t.Fatalf("staged=%v err=%v", staged, err)
	}
	if restarted.Load() {
		t.Error("handed off outside the maintenance window")
	}
	if staged, _ := a.Tick(context.Background()); !staged {
		t.Error("staged update was lost between ticks")
	}
}

func TestTickRejectsChecksumMismatch(t *testing.T) {
	base := t.TempDir()
	art, _ := makeArtifact(t, t.TempDir(), "core 1.1.0")
	bad := hex.EncodeToString(sha256.New().Sum(nil))
	srv := stubCloud(t, &Release{Version: "1.1.0", ArtifactURL: "/artifacts/a.zip", ChecksumSHA256: bad, RolloutPct: 100}, art)
	a := newTestAgent(t, base, srv.URL, AnyTime, func() { t.Error("handed off after a checksum mismatch") })
	if _, err := a.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if _, err := os.Stat(launcher.CoreBinaryPath(base, "1.1.0")); err == nil {
		t.Error("a corrupt artifact was staged")
	}
}

func TestTickIgnoresOlderAndOutOfRolloutReleases(t *testing.T) {
	base := t.TempDir()
	art, sum := makeArtifact(t, t.TempDir(), "old")
	for _, rel := range []*Release{
		{Version: "0.9.0", ArtifactURL: "/artifacts/a.zip", ChecksumSHA256: sum, RolloutPct: 100},
		{Version: "2.0.0", ArtifactURL: "/artifacts/a.zip", ChecksumSHA256: sum, RolloutPct: 0},
	} {
		srv := stubCloud(t, rel, art)
		a := newTestAgent(t, base, srv.URL, AnyTime, func() { t.Errorf("handed off for %s", rel.Version) })
		if staged, err := a.Tick(context.Background()); staged || err != nil {
			t.Errorf("%s: staged=%v err=%v", rel.Version, staged, err)
		}
	}
}

func TestUnsupervisedNeverExits(t *testing.T) {
	base := t.TempDir()
	art, sum := makeArtifact(t, t.TempDir(), "core")
	srv := stubCloud(t, &Release{Version: "1.1.0", ArtifactURL: "/artifacts/a.zip", ChecksumSHA256: sum, RolloutPct: 100}, art)
	id, _ := ident.GenerateIdentity()
	a := New(Config{
		Logger: quiet(), Identity: id, Version: "1.0.0", CloudURL: srv.URL, BaseDir: base,
		Supervised: false, Window: AnyTime,
		RequestRestart: func() { t.Error("an unsupervised core must not exit for an update") },
	})
	if _, err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNightWindow(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 9, 12, h, 30, 0, 0, time.Local) }
	for _, h := range []int{2, 3, 4} {
		if !NightWindow(at(h)) {
			t.Errorf("%02d:30 should be inside the window", h)
		}
	}
	for _, h := range []int{1, 5, 12, 23} {
		if NightWindow(at(h)) {
			t.Errorf("%02d:30 should be outside the window", h)
		}
	}
}

func TestCheckRejectsAnInvalidManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"version":"../../evil","artifact_url":"/a.zip","checksum_sha256":"x","rollout_pct":100}`)
	}))
	defer srv.Close()
	a := newTestAgent(t, t.TempDir(), srv.URL, AnyTime, nil)
	if _, err := a.Tick(context.Background()); err == nil {
		t.Error("a manifest with a path-traversal version was accepted")
	}
}
