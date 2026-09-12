//go:build windows

package launcher

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// The core must die with the launcher (no orphan holding the port).
func TestJobObjectKillsChildOnClose(t *testing.T) {
	cmd := exec.Command(buildChild(t))
	stdin, _ := cmd.StdinPipe() // held open: the child would otherwise run for an hour
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	job, err := newJobObject()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.assign(cmd.Process); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	job.close()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child survived the job object being closed")
	}
}

func TestServiceHandlerStops(t *testing.T) {
	stopped := make(chan struct{})
	h := &serviceHandler{
		run:      func(ctx context.Context) error { <-ctx.Done(); close(stopped); return nil },
		stopWait: 5 * time.Second,
	}
	req := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 10)
	result := make(chan uint32, 1)
	go func() { _, code := h.Execute(nil, req, status); result <- code }()

	if s := <-status; s.State != svc.StartPending {
		t.Fatalf("first state %v", s.State)
	}
	if s := <-status; s.State != svc.Running || s.Accepts&svc.AcceptStop == 0 {
		t.Fatalf("second state %+v", s)
	}
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	if s := <-status; s.State != svc.StopPending {
		t.Fatalf("after stop: %v", s.State)
	}
	select {
	case code := <-result:
		if code != 0 {
			t.Errorf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after Stop")
	}
	select {
	case <-stopped:
	default:
		t.Error("supervisor context was not cancelled")
	}
}
