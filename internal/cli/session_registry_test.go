package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterSession_WritesExpectedShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := RegisterSession("/some/project/dir", "/run/user/1000/cc-socks/123.sock")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}

	wantPath := filepath.Join(home, ".pad", "sessions", fmt.Sprintf("%d.json", os.Getpid()))
	if path != wantPath {
		t.Fatalf("expected path %q, got %q", wantPath, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registration file: %v", err)
	}
	var reg SessionRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("parse registration: %v", err)
	}
	if reg.PID != os.Getpid() {
		t.Errorf("expected pid %d, got %d", os.Getpid(), reg.PID)
	}
	if reg.Cwd != "/some/project/dir" {
		t.Errorf("expected cwd '/some/project/dir', got %q", reg.Cwd)
	}
	if reg.MessagingSocketPath != "/run/user/1000/cc-socks/123.sock" {
		t.Errorf("expected messaging socket path to round-trip, got %q", reg.MessagingSocketPath)
	}
	if reg.RegisteredAt == "" {
		t.Error("expected a non-empty RegisteredAt")
	}
}

func TestRegisterSession_EmptySocketPathOmitted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := RegisterSession("/some/dir", "")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := raw["messaging_socket_path"]; ok {
		t.Error("expected messaging_socket_path key to be omitted when empty")
	}
}

func TestRegisterSession_FilePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := RegisterSession("/some/dir", "")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected mode 0600, got %o", info.Mode().Perm())
	}
}

func TestRegisterSession_ReRegisterOverwrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := RegisterSession("/first/dir", ""); err != nil {
		t.Fatalf("first RegisterSession: %v", err)
	}
	path, err := RegisterSession("/second/dir", "")
	if err != nil {
		t.Fatalf("second RegisterSession: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var reg SessionRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reg.Cwd != "/second/dir" {
		t.Fatalf("expected re-registration to overwrite cwd, got %q", reg.Cwd)
	}
}
