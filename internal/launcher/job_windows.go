//go:build windows

package launcher

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObject kills every assigned process when the launcher's handle is
// closed — including when the launcher itself is killed — so the core can
// never be orphaned holding the dashboard port.
type jobObject struct{ h windows.Handle }

func newJobObject() (*jobObject, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &jobObject{h: h}, nil
}

func (j *jobObject) assign(p *os.Process) error {
	hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(hp)
	return windows.AssignProcessToJobObject(j.h, hp)
}

func (j *jobObject) close() { _ = windows.CloseHandle(j.h) }
