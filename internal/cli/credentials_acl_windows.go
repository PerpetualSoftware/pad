//go:build windows

package cli

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// restrictCredentialsFile replaces path's DACL with a protected DACL granting
// GENERIC_ALL only to the current process token's user SID (BUG-3807).
//
// It opens its own handle with full sharing instead of calling
// SetNamedSecurityInfo so open readers of credentials.json are not disturbed
// (see credentials_touch_windows.go).
func restrictCredentialsFile(path string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	var pinner runtime.Pinner
	pinner.Pin(tu.User.Sid)
	defer pinner.Unpin()

	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(tu.User.Sid),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
		p,
		windows.WRITE_DAC|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)

	return windows.SetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

// credentialsFileOwnedByUs reports whether path's owner SID is the current
// process token's user SID or its default owner SID (BUG-3807). Any error returns false so a warning is
// not emitted when ownership cannot be established.
func credentialsFileOwnedByUs(path string) bool {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tu == nil || tu.User.Sid == nil {
		return false
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(
		p,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	if owner.Equals(tu.User.Sid) {
		return true
	}
	// An elevated administrator's token makes BUILTIN\Administrators, not the
	// user, the owner of the files it creates (its TokenOwner), so a file this
	// process created is ours under that SID too.
	defOwner, err := tokenDefaultOwner(windows.GetCurrentProcessToken())
	return err == nil && owner.Equals(defOwner)
}

// tokenDefaultOwner returns a copy of the token's TokenOwner SID, the owner
// given to objects the token creates. x/sys/windows has no getter for it.
func tokenDefaultOwner(t windows.Token) (*windows.SID, error) {
	n := uint32(64)
	for {
		b := make([]byte, n)
		err := windows.GetTokenInformation(t, windows.TokenOwner, &b[0], uint32(len(b)), &n)
		if err == nil {
			// TOKEN_OWNER is a single *SID pointing into b.
			return (*struct{ Owner *windows.SID })(unsafe.Pointer(&b[0])).Owner.Copy()
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER || n <= uint32(len(b)) {
			return nil, err
		}
	}
}
