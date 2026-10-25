//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mkfifoForTest(path string) error { return syscall.Mkfifo(path, 0600) }

// A FIFO with a reader opens fine, so only the regular-file check stops
// the log writing into someone's pipe.
func TestChatSeenRefusesFifoWithReader(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, _ := ChatSeenPath(12)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Skip("mkfifo:", err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var warn bytes.Buffer
	OpenChatSeen(chatSeenOwner(12), nil, &warn).Append("x")
	if !strings.Contains(warn.String(), "not a regular file") {
		t.Fatalf("warn %q", warn.String())
	}
	buf := make([]byte, 8)
	if n, _ := r.Read(buf); n != 0 {
		t.Fatalf("wrote %q into a FIFO", buf[:n])
	}
}
