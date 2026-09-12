//go:build windows

package launcher

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
)

// IsWindowsService reports whether the process was started by the
// Service Control Manager.
func IsWindowsService() (bool, error) { return svc.IsWindowsService() }

// RunService runs run(ctx) as the Windows service `name` until the SCM
// asks it to stop.
func RunService(name string, run func(ctx context.Context) error) error {
	return svc.Run(name, &serviceHandler{run: run, stopWait: 25 * time.Second})
}

type serviceHandler struct {
	run      func(ctx context.Context) error
	stopWait time.Duration
}

// Execute implements svc.Handler.
func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			// The supervisor gave up (e.g. nothing installed). Report a
			// service-specific failure so SCM recovery actions apply.
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, WaitHint: uint32(h.stopWait / time.Millisecond)}
				cancel()
				select {
				case <-done:
				case <-time.After(h.stopWait):
				}
				return false, 0
			}
		}
	}
}
