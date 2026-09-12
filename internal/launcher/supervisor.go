// Package launcher is the Windows service process. It runs the current
// FuelMindCore.exe as a child, restarts it with backoff, swaps to a staged
// update when the core asks for it (exit code 42), checks the new
// version's /healthz, and rolls back on a failed self-check or crash loop.
//
// Directory layout under the data dir (C:\ProgramData\FuelMind):
//
//	current.json / previous.json / pending.json   version pointers
//	versions\<v>\FuelMindCore.exe                  installed versions
//	logs\core.log, logs\launcher.log               output (rotated at 10 MB)
//	data\rollback_occurred.json                    rollback to report
package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExitCodeRequestRestart is the core's exit code for "a staged update is
// ready; hand off to it".
const ExitCodeRequestRestart = 42

const (
	crashLoopMaxCrashes = 3
	crashLoopWindow     = 10 * time.Minute
	maxBackoff          = time.Minute
	stableRun           = 2 * time.Minute
	logRotateBytes      = 10 << 20
)

// Options configures a Supervisor.
type Options struct {
	BaseDir       string        // data dir (required)
	InstallDir    string        // directory holding the installer's FuelMindCore.exe; "" disables bootstrap
	Port          int           // dashboard port used for the post-update health check (default 8765)
	HealthTimeout time.Duration // how long a new version has to answer /healthz (default 90s)
	Probation     time.Duration // window after an update in which a crash triggers rollback (default 2h)
	Logger        *log.Logger
}

// Supervisor runs and supervises the core.
type Supervisor struct {
	opt      Options
	log      *log.Logger
	detector *CrashLoopDetector
	job      *jobObject

	probationUntil   time.Time
	probationCrashes int
}

// NewSupervisor builds a supervisor with default options for baseDir.
func NewSupervisor(baseDir string) *Supervisor { return New(Options{BaseDir: baseDir}) }

// New builds a supervisor.
func New(opt Options) *Supervisor {
	if opt.Port == 0 {
		opt.Port = 8765
	}
	if opt.HealthTimeout == 0 {
		opt.HealthTimeout = 90 * time.Second
	}
	if opt.Probation == 0 {
		opt.Probation = 2 * time.Hour
	}
	if opt.Logger == nil {
		opt.Logger = log.New(os.Stderr, "launcher: ", log.LstdFlags)
	}
	return &Supervisor{
		opt:      opt,
		log:      opt.Logger,
		detector: NewCrashLoopDetector(crashLoopMaxCrashes, crashLoopWindow),
	}
}

var errNoCurrentVersion = errors.New("launcher: no current version set (no current.json and no FuelMindCore.exe next to the launcher)")

// Run supervises the core until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.opt.InstallDir != "" {
		v, err := Bootstrap(s.opt.BaseDir, s.opt.InstallDir)
		if err != nil {
			s.log.Printf("bootstrap from %s failed: %v", s.opt.InstallDir, err)
		} else if v != "" {
			s.log.Printf("installed core %s from %s", v, s.opt.InstallDir)
		}
	}
	current, err := ReadCurrentVersion(s.opt.BaseDir)
	if err != nil {
		return err
	}
	if current == "" {
		return errNoCurrentVersion
	}

	job, err := newJobObject()
	if err != nil {
		s.log.Printf("job object unavailable (child may outlive the launcher): %v", err)
	} else {
		s.job = job
		defer job.close()
	}

	backoff := time.Second
	for ctx.Err() == nil {
		if _, err := os.Stat(CoreBinaryPath(s.opt.BaseDir, current)); err != nil {
			s.log.Printf("core binary for %s is missing: %v", current, err)
			if prev, ok := s.rollback(current, "core binary missing"); ok {
				current = prev
				continue
			}
			sleepCtx(ctx, 30*time.Second)
			continue
		}

		inProbation := time.Now().Before(s.probationUntil)
		started := time.Now()
		exitCode, runErr, unhealthy := s.runChild(ctx, current, inProbation)
		if ctx.Err() != nil {
			return nil
		}

		switch {
		case unhealthy:
			if prev, ok := s.rollback(current, fmt.Sprintf("self-check failed: /healthz did not answer within %s after the update", s.opt.HealthTimeout)); ok {
				current = prev
			}
			continue

		case exitCode == ExitCodeRequestRestart:
			pending, has, perr := ReadPending(s.opt.BaseDir)
			if perr != nil || !has {
				s.log.Printf("core requested a handoff but there is no valid pending.json (%v); restarting %s", perr, current)
				continue
			}
			if _, err := os.Stat(CoreBinaryPath(s.opt.BaseDir, pending)); err != nil {
				s.log.Printf("pending version %s has no binary: %v", pending, err)
				_ = DeletePending(s.opt.BaseDir)
				continue
			}
			if err := s.switchTo(pending, current); err != nil {
				s.log.Printf("failed to switch to %s: %v", pending, err)
				continue
			}
			s.log.Printf("switched %s -> %s; probation until %s", current, pending, time.Now().Add(s.opt.Probation).Format(time.RFC3339))
			current = pending
			s.detector.Reset()
			s.probationUntil = time.Now().Add(s.opt.Probation)
			s.probationCrashes = 0
			backoff = time.Second
			continue
		}

		crashed := runErr != nil || exitCode != 0
		if crashed {
			s.log.Printf("core %s exited (code %d, err %v) after %s", current, exitCode, runErr, time.Since(started).Round(time.Second))
			if inProbation {
				s.probationCrashes++
				if s.probationCrashes >= 2 {
					if prev, ok := s.rollback(current, "crashed repeatedly during post-update probation"); ok {
						current = prev
						continue
					}
				}
			}
			if s.detector.RecordCrash(time.Now()) {
				s.log.Printf("crash loop detected on %s", current)
				if prev, ok := s.rollback(current, "crash loop"); ok {
					current = prev
					continue
				}
				// Nowhere to roll back to: slow down instead of hot-looping.
				backoff = maxBackoff
			}
		} else {
			s.log.Printf("core %s exited cleanly; restarting", current)
		}
		if time.Since(started) > stableRun {
			backoff = time.Second
		}
		sleepCtx(ctx, backoff)
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return nil
}

// runChild runs one core process and returns its exit code. When
// checkHealth is set it polls /healthz and stops the child (unhealthy =
// true) if it does not answer within HealthTimeout.
func (s *Supervisor) runChild(ctx context.Context, v string, checkHealth bool) (exitCode int, err error, unhealthy bool) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	bin := CoreBinaryPath(s.opt.BaseDir, v)
	cmd := exec.CommandContext(childCtx, bin)
	cmd.Dir = filepath.Dir(bin)
	cmd.Env = append(os.Environ(),
		"FUELMIND_DATA_DIR="+s.opt.BaseDir,
		"FUELMIND_SUPERVISED=1",
		"FUELMIND_PORT="+strconv.Itoa(s.opt.Port),
	)
	stdin, perr := cmd.StdinPipe()
	if perr != nil {
		return -1, perr, false
	}
	// Graceful stop: the core shuts down when its stdin closes; it is
	// killed if still running WaitDelay later.
	cmd.Cancel = func() error { return stdin.Close() }
	cmd.WaitDelay = 15 * time.Second

	logw, lerr := s.openLog("core.log")
	if lerr == nil {
		defer logw.Close()
		cmd.Stdout, cmd.Stderr = logw, logw
	} else {
		s.log.Printf("cannot open core log: %v", lerr)
	}

	if err := cmd.Start(); err != nil {
		return -1, err, false
	}
	if s.job != nil {
		if err := s.job.assign(cmd.Process); err != nil {
			s.log.Printf("assign core to job object: %v", err)
		}
	}

	healthFailed := make(chan struct{})
	if checkHealth {
		until := s.probationUntil
		go func() {
			if !waitHealthy(childCtx, s.opt.Port, s.opt.HealthTimeout) {
				if childCtx.Err() == nil {
					close(healthFailed)
					cancel()
				}
				return
			}
			s.log.Printf("%s passed its /healthz self-check", v)
			sleepCtx(childCtx, time.Until(until))
			if childCtx.Err() == nil {
				s.log.Printf("%s survived probation; update committed", v)
				if err := PruneVersions(s.opt.BaseDir); err != nil {
					s.log.Printf("prune old versions: %v", err)
				}
			}
		}()
	}

	werr := cmd.Wait()
	select {
	case <-healthFailed:
		return -1, werr, true
	default:
	}
	var exitErr *exec.ExitError
	if errors.As(werr, &exitErr) {
		return exitErr.ExitCode(), nil, false
	}
	if werr != nil {
		return -1, werr, false
	}
	return 0, nil, false
}

func waitHealthy(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if resp, err := client.Get(url); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		sleepCtx(ctx, 2*time.Second)
	}
	return false
}

// switchTo performs the pointer swap for a normal update.
func (s *Supervisor) switchTo(newVersion, oldVersion string) error {
	if err := WritePreviousVersion(s.opt.BaseDir, oldVersion); err != nil {
		return err
	}
	if err := WriteCurrentVersion(s.opt.BaseDir, newVersion); err != nil {
		return err
	}
	return DeletePending(s.opt.BaseDir)
}

// rollback points current back at previous and records why, so the core
// reports it in the next heartbeat. It returns false when there is no
// usable previous version.
func (s *Supervisor) rollback(failed, reason string) (string, bool) {
	prev, err := ReadPreviousVersion(s.opt.BaseDir)
	if err != nil || prev == "" || prev == failed {
		s.log.Printf("cannot roll back from %s (%s): no previous version", failed, reason)
		return "", false
	}
	if _, err := os.Stat(CoreBinaryPath(s.opt.BaseDir, prev)); err != nil {
		s.log.Printf("cannot roll back to %s: %v", prev, err)
		return "", false
	}
	if err := WriteCurrentVersion(s.opt.BaseDir, prev); err != nil {
		s.log.Printf("rollback pointer write failed: %v", err)
		return "", false
	}
	_ = DeletePending(s.opt.BaseDir)
	if err := quarantineVersion(s.opt.BaseDir, failed, reason); err != nil {
		s.log.Printf("quarantine %s: %v", failed, err)
	}
	if err := writeRollbackRecord(s.opt.BaseDir, failed, reason); err != nil {
		s.log.Printf("write rollback record: %v", err)
	}
	s.log.Printf("rolled back %s -> %s: %s", failed, prev, reason)
	s.detector.Reset()
	s.probationUntil = time.Time{}
	s.probationCrashes = 0
	return prev, true
}

// PruneVersions deletes installed versions other than current, previous
// and pending.
func PruneVersions(baseDir string) error {
	keep := map[string]bool{}
	for _, read := range []func(string) (string, error){ReadCurrentVersion, ReadPreviousVersion} {
		if v, err := read(baseDir); err == nil && v != "" {
			keep[v] = true
		}
	}
	if v, ok, _ := ReadPending(baseDir); ok {
		keep[v] = true
	}
	entries, err := os.ReadDir(VersionsDir(baseDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && !keep[e.Name()] && !strings.HasPrefix(e.Name(), ".tmp-") {
			if err := os.RemoveAll(filepath.Join(VersionsDir(baseDir), e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// openLog opens logs\<name> for appending, rotating it at 10 MB.
func (s *Supervisor) openLog(name string) (*os.File, error) {
	return OpenRotatingLog(filepath.Join(s.opt.BaseDir, "logs"), name)
}

// OpenRotatingLog opens dir\name for appending; a file over 10 MB is
// renamed to name.1 first.
func OpenRotatingLog(dir, name string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	if fi, err := os.Stat(p); err == nil && fi.Size() > logRotateBytes {
		_ = os.Remove(p + ".1")
		_ = os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
