//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUserAndEveryoneSIDs(t *testing.T) (userSID, everyoneSID *windows.SID) {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(WinWorldSid): %v", err)
	}
	return tu.User.Sid, everyone
}

func grantUserAndEveryone(t *testing.T, target string, userSID, everyoneSID *windows.SID, userInherit, everyoneInherit uint32, secInfo windows.SECURITY_INFORMATION) {
	t.Helper()
	var pinner runtime.Pinner
	pinner.Pin(userSID)
	pinner.Pin(everyoneSID)
	defer pinner.Unpin()

	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       userInherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(userSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_READ,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       everyoneInherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(everyoneSID),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries(%s): %v", target, err)
	}
	if err := windows.SetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, secInfo, nil, nil, acl, nil); err != nil {
		t.Fatalf("SetNamedSecurityInfo(%s): %v", target, err)
	}
}

func assertOwnerOnlyDACL(t *testing.T, path string, userSID, everyoneSID *windows.SID) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatalf("sd.Control: %v", err)
	}
	if ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("SE_DACL_PROTECTED is not set (control = %#x)", ctrl)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("sd.DACL: %v", err)
	}
	if dacl == nil {
		t.Fatal("credentials.json has a nil (allow-all) DACL")
	}
	if dacl.AceCount != 1 {
		t.Errorf("DACL AceCount = %d, want 1", dacl.AceCount)
	}
	var sawUser bool
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("GetAce(%d): %v", i, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("ACE %d type = %d, want ACCESS_ALLOWED_ACE_TYPE (%d)", i, ace.Header.AceType, windows.ACCESS_ALLOWED_ACE_TYPE)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(everyoneSID) {
			t.Errorf("ACE %d grants access to Everyone (WinWorldSid)", i)
		}
		if sid.Equals(userSID) {
			sawUser = true
		} else {
			t.Errorf("ACE %d names unexpected SID %v", i, sid)
		}
	}
	if !sawUser {
		t.Errorf("DACL has no ACE for the current user SID %v", userSID)
	}
}

// BUG-3807: on Windows, credentials.json must get a protected owner-only DACL
// rather than inheriting a readable ACE from ~/.pad.
func TestCredentialsWindowsSaveOwnerOnlyDACL(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("USERPROFILE", home)

	padDir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(padDir, 0700); err != nil {
		t.Fatalf("mkdir .pad: %v", err)
	}
	userSID, everyoneSID := currentUserAndEveryoneSIDs(t)
	grantUserAndEveryone(
		t,
		padDir,
		userSID,
		everyoneSID,
		windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		windows.OBJECT_INHERIT_ACE,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)

	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "secret"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(padDir, "credentials.json")
	assertOwnerOnlyDACL(t, path, userSID, everyoneSID)
}

// BUG-3807: an existing credentials.json carrying an explicit Everyone-read ACE
// must be tightened to a protected owner-only DACL on Save.
func TestCredentialsWindowsExistingLooseFileTightened(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("USERPROFILE", home)

	padDir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(padDir, 0700); err != nil {
		t.Fatalf("mkdir .pad: %v", err)
	}
	userSID, everyoneSID := currentUserAndEveryoneSIDs(t)
	grantUserAndEveryone(
		t,
		padDir,
		userSID,
		everyoneSID,
		windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		windows.OBJECT_INHERIT_ACE,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)

	path := filepath.Join(padDir, "credentials.json")
	if err := os.WriteFile(path, []byte(`{"version": 2, "credentials": {}}`), 0600); err != nil {
		t.Fatalf("seed credentials.json: %v", err)
	}
	grantUserAndEveryone(
		t,
		path,
		userSID,
		everyoneSID,
		windows.NO_INHERITANCE,
		windows.NO_INHERITANCE,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
	)

	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "fresh"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	assertOwnerOnlyDACL(t, path, userSID, everyoneSID)
}
