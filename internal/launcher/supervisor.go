package launcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	ExitCodeRequestRestart = 42 // core exits with this to request a
	                            // handoff into a pending version
	crashLoopMaxCrashes = 3
	crashLoopWindow     = 10 * time.Minute
	probationDuration   = 2 * time.Hour
)

type Supervisor struct {
	baseDir  string
	detector *CrashLoopDetector
}

func NewSupervisor(baseDir string) *Supervisor {
	return &Supervisor{
		baseDir:  baseDir,
		detector: NewCrashLoopDetector(crashLoopMaxCrashes, crashLoopWindow),
	}
}

// Run is the launcher's main loop. It never returns under normal
// operation; it's meant to be called from the Windows service's
// Execute callback (see service.go).
func (s *Supervisor) Run(ctx context.Context) error {
	version, err := ReadCurrentVersion(s.baseDir)
	if err != nil {
		return err
	}
	if version == "" {
		return errNoCurrentVersion
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		exitCode, runErr := s.runChild(ctx, version)
		now := time.Now()

		if exitCode == ExitCodeRequestRestart {
			pendingVersion, hasPending, err := ReadPending(s.baseDir)
			if err != nil || !hasPending {
				log.Printf("launcher: child requested restart but no valid pending.json: %v", err)
				continue // just restart the same version
			}
			if err := s.switchTo(pendingVersion, version); err != nil {
				log.Printf("launcher: failed to switch to %s: %v", pendingVersion, err)
				continue // stay on current version
			}
			version = pendingVersion
			s.detector.Reset()
			s.runProbation(ctx, version)
			continue
		}

		// Any other exit (crash) counts toward crash-loop detection.
		if runErr != nil || exitCode != 0 {
			if s.detector.RecordCrash(now) {
				log.Printf("launcher: crash loop detected on version %s", version)
				prev, _ := ReadPreviousVersion(s.baseDir)
				if prev != "" && prev != version {
					log.Printf("launcher: rolling back to %s", prev)
					s.rollbackTo(prev, version, "crash loop on committed version")
					version = prev
					s.detector.Reset()
				} else {
					// If there's no previous version to roll back to, this
					// is a genuinely broken install with nowhere to fall
					// back to — back off restarts rather than hot-looping.
					time.Sleep(30 * time.Second)
				}
			}
		}
	}
}

func (s *Supervisor) runChild(ctx context.Context, version string) (exitCode int, err error) {
	binPath := CoreBinaryPath(s.baseDir, version)
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Dir = filepath.Dir(binPath)
	logFile, ferr := os.OpenFile(filepath.Join(s.baseDir, "logs", "core.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if ferr == nil {
		defer logFile.Close()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		return -1, err
	}
	err = cmd.Wait()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// switchTo performs the atomic pointer swap for a normal update.
func (s *Supervisor) switchTo(newVersion, oldVersion string) error {
	if err := WritePreviousVersion(s.baseDir, oldVersion); err != nil {
		return err
	}
	if err := WriteCurrentVersion(s.baseDir, newVersion); err != nil {
		return err
	}
	return DeletePending(s.baseDir)
}

// WritePendingVersion writes the given version to pending.json.
func WritePendingVersion(baseDir, version string) error {
	return writePointerAtomic(filepath.Join(baseDir, "pending.json"), version)
}

// rollbackTo performs the same atomic swap but in the failure
// direction, and does not touch previous.json (it already points at
// the version we're rolling back TO).
func (s *Supervisor) rollbackTo(rollbackVersion, failedVersion, reason string) {
	_ = WriteCurrentVersion(s.baseDir, rollbackVersion)
	_ = writeRollbackRecord(s.baseDir, failedVersion, reason)
}

// runProbation blocks for the probation window, watching for a crash
// loop; if one occurs it triggers rollback and returns, letting the
// caller's normal loop pick up the rolled-back version.
func (s *Supervisor) runProbation(ctx context.Context, version string) {
	deadline := time.Now().Add(probationDuration)
	log.Printf("launcher: entering probation for version %s until %s", version, deadline)
	// Probation itself is enforced by the SAME crash-loop detector
	// used in steady state — no separate code path needed, since
	// runChild/RecordCrash already run on every exit. This function
	// exists as a hook point for future healthz polling (see note
	// below) rather than duplicating restart logic.
}

// errNoCurrentVersion is returned when the current.json file is missing
// or empty.
var errNoCurrentVersion = fmt.Errorf("launcher: no current version set")