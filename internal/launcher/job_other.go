//go:build !windows

package launcher

import "os"

// jobObject is a no-op outside Windows (the launcher is Windows-only in
// production; this keeps the package buildable for CI on Linux).
type jobObject struct{}

func newJobObject() (*jobObject, error)       { return &jobObject{}, nil }
func (j *jobObject) assign(*os.Process) error { return nil }
func (j *jobObject) close()                   {}
