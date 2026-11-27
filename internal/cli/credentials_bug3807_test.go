package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-3807: when restricting access to credentials.json fails on a file we own,
// Save emits a single warning naming the path and still writes the credentials.
func TestCredentialsRestrictFailureWarnsAndSaves(t *testing.T) {
	home := withTempHome(t)
	t.Setenv("USERPROFILE", home)

	restrictErr := errors.New("simulated ACL failure")
	origRestrict := restrictCredentials
	restrictCredentials = func(string) error { return restrictErr }
	t.Cleanup(func() { restrictCredentials = origRestrict })

	var warn strings.Builder
	origWarn := credentialsLockWarning
	credentialsLockWarning = &warn
	t.Cleanup(func() { credentialsLockWarning = origWarn })

	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "saved-despite-acl-err"})
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(home, ".pad", "credentials.json")
	gotWarn := warn.String()
	wantWarn := fmt.Sprintf("warning: could not restrict access to %s to the current user (%v); saving credentials anyway\n", path, restrictErr)
	if gotWarn != wantWarn {
		t.Errorf("warning = %q, want %q", gotWarn, wantWarn)
	}
	lines := strings.Split(strings.TrimSuffix(gotWarn, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("warning had %d lines, want 1: %q", len(lines), gotWarn)
	}

	loaded, err := LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if c := loaded.Get("http://x:7777"); c == nil || c.Token != "saved-despite-acl-err" {
		t.Errorf("LoadStore got %+v, want saved-despite-acl-err", c)
	}
}
