//go:build !windows

package launcher

import (
	"context"
	"errors"
)

// IsWindowsService is always false outside Windows.
func IsWindowsService() (bool, error) { return false, nil }

// RunService is only supported on Windows.
func RunService(string, func(ctx context.Context) error) error {
	return errors.New("launcher: Windows services are only supported on Windows")
}
