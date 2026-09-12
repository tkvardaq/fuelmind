package launcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/version"
)

// Bootstrap adopts the core that the installer placed next to the
// launcher (installDir\FuelMindCore.exe) into baseDir\versions\<v>\.
//
//   - Fresh install: the bundled core becomes current.
//   - Installer upgrade (bundled core is newer than current): the old
//     current becomes previous and the bundled core becomes current.
//   - Bundled core older than what unattended updates already installed:
//     it is stored but not activated.
//
// It returns the adopted version ("" when nothing changed).
func Bootstrap(baseDir, installDir string) (string, error) {
	src := filepath.Join(installDir, CoreExeName)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	v, err := probeVersion(src)
	if err != nil {
		return "", err
	}
	cur, err := ReadCurrentVersion(baseDir)
	if err != nil {
		return "", err
	}
	dst := CoreBinaryPath(baseDir, v)
	if _, err := os.Stat(dst); err == nil {
		if cur == "" {
			return v, WriteCurrentVersion(baseDir, v)
		}
		return "", nil
	}
	if err := installVersion(baseDir, v, src); err != nil {
		return "", err
	}
	switch {
	case cur == "":
		return v, WriteCurrentVersion(baseDir, v)
	case version.Compare(v, cur) > 0:
		if err := WritePreviousVersion(baseDir, cur); err != nil {
			return "", err
		}
		if err := DeletePending(baseDir); err != nil {
			return "", err
		}
		return v, WriteCurrentVersion(baseDir, v)
	}
	return "", nil
}

// probeVersion runs "<core> -version" and parses "fuelmind-core <v>".
func probeVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, "-version")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("launcher: %s -version: %w", exe, err)
	}
	fields := strings.Fields(out.String())
	if len(fields) != 2 || fields[0] != "fuelmind-core" || !version.Valid(fields[1]) {
		return "", fmt.Errorf("launcher: unexpected -version output %q", out.String())
	}
	return fields[1], nil
}

// installVersion copies exe into versions\<v>\ via a temp dir + rename.
func installVersion(baseDir, v, exe string) error {
	tmp := filepath.Join(VersionsDir(baseDir), ".tmp-"+v)
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := copyFile(exe, filepath.Join(tmp, CoreExeName)); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	final := filepath.Join(VersionsDir(baseDir), v)
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
