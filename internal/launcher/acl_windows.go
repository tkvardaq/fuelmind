//go:build windows

package launcher

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Well-known SIDs, used instead of account names so the code works on a
// localized Windows.
const (
	sidLocalService  = "S-1-5-19"
	sidSystem        = "S-1-5-18"
	sidAdmins        = "S-1-5-32-544"
	sidAuthedUsers   = "S-1-5-11"
	posDropFolder    = "pos_drop"
	modifyPermission = windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.DELETE
)

// HardenDataDir restricts the data directory to the service account,
// SYSTEM and Administrators, breaking the inheritance that otherwise lets
// every local user read the station's sales history (%ProgramData% grants
// Users read by default).
//
// The POS drop folder keeps write access for authenticated users, because
// the POS export runs as an ordinary account.
//
// It is applied on every service start, so a folder restored from backup
// or copied to a new PC is protected too. A failure is not fatal: the
// caller logs it and carries on.
func HardenDataDir(dir string) error {
	if err := setProtectedACL(dir, false); err != nil {
		return err
	}
	return setProtectedACL(filepath.Join(dir, posDropFolder), true)
}

func setProtectedACL(path string, allowAuthenticatedUsers bool) error {
	trustees := []struct {
		sid    string
		rights uint32
	}{
		{sidLocalService, windows.GENERIC_ALL},
		{sidSystem, windows.GENERIC_ALL},
		{sidAdmins, windows.GENERIC_ALL},
	}
	if allowAuthenticatedUsers {
		trustees = append(trustees, struct {
			sid    string
			rights uint32
		}{sidAuthedUsers, modifyPermission})
	}

	entries := make([]windows.EXPLICIT_ACCESS, 0, len(trustees))
	for _, t := range trustees {
		sid, err := windows.StringToSid(t.sid)
		if err != nil {
			return fmt.Errorf("sid %s: %w", t.sid, err)
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.ACCESS_MASK(t.rights),
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build acl for %s: %w", path, err)
	}
	// PROTECTED_DACL_SECURITY_INFORMATION drops inherited entries.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("set acl on %s: %w", path, err)
	}
	return nil
}
