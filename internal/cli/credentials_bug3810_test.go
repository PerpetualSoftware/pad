package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// BUG-3810: a failed stat used to read as "not a regular file", which skips
// the truncate, so a shorter body left the old tail behind. It now fails the
// write before anything is written.
func TestBUG3810_FailedStatFailsTheWriteAndLeavesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	old := `{"credentials": {"a long body that a shorter write would leave a tail of": {}}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	statErr := errors.New("stat failed")
	orig := statOpenFile
	statOpenFile = func(*os.File) (os.FileInfo, error) { return nil, statErr }
	t.Cleanup(func() { statOpenFile = orig })

	if err := writeFileInPlace0600(path, []byte(`{}`)); !errors.Is(err, statErr) {
		t.Fatalf("write with a failed stat: err = %v, want the stat error", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != old {
		t.Fatalf("a refused write changed the file to %q", got)
	}

	// CONTROL: with the stat working, the shorter body replaces the file whole.
	statOpenFile = orig
	if err := writeFileInPlace0600(path, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{}` {
		t.Fatalf("a working write left %q, want {}", got)
	}
}
