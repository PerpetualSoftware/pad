//go:build unix

package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-3807: a permission failure on a foreign-owned credentials.json (or its
// lock file, e.g. left root-owned by `sudo pad auth login`) keeps wrapping
// fs.ErrPermission and adds an actionable sudo chown hint.
func TestCredentialsForeignOwnerHint(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file mode 000")
	}

	home := withTempHome(t)
	store := newEmptyStore()
	store.Set("http://x:7777", &Credentials{Token: "t"})
	if err := store.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	path := filepath.Join(home, ".pad", "credentials.json")
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })

	origUID := currentUID
	t.Cleanup(func() { currentUID = origUID })

	// Simulated foreign owner: uid differs from the file's owner.
	currentUID = func() int { return origUID() + 1 }

	assertHint := func(label string, err error) {
		t.Helper()
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("%s: err = %v, want errors.Is(_, fs.ErrPermission)", label, err)
		}
		msg := err.Error()
		for _, want := range []string{path, ".lock", "sudo chown"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: error %q missing %q", label, msg, want)
			}
		}
	}

	_, err := LoadStore()
	assertHint("LoadStore (foreign)", err)

	err = UpdateStore(func(*CredentialStore) error { return nil })
	assertHint("UpdateStore (foreign)", err)

	err = store.Save()
	assertHint("Save (foreign)", err)

	_, err = loadStoreUnlocked(path)
	assertHint("loadStoreUnlocked (foreign)", err)

	// Owned by the current user: permission error must NOT claim foreign ownership.
	currentUID = origUID

	assertNoHint := func(label string, err error) {
		t.Helper()
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("%s: err = %v, want errors.Is(_, fs.ErrPermission)", label, err)
		}
		if strings.Contains(err.Error(), "sudo chown") {
			t.Errorf("%s: unexpected sudo chown hint for own file: %q", label, err.Error())
		}
	}

	_, err = LoadStore()
	assertNoHint("LoadStore (own)", err)

	err = UpdateStore(func(*CredentialStore) error { return nil })
	assertNoHint("UpdateStore (own)", err)

	err = store.Save()
	assertNoHint("Save (own)", err)
}
