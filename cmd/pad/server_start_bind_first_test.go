package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TASK-3620. A `pad server start` that loses the port must exit without having
// touched shared state. The CLI's auto-start spawned 206 losing starts in two
// days, and each opened the live database and started its background workers
// before the bind failed. The start now binds first, so a held port refuses it
// before the database is opened: here, before the SQLite file is even created.
func TestServerStart_LosingTheBindOpensNoDatabase(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "pad.db")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", dataDir)
	t.Setenv("PAD_DB_PATH", dbPath)
	t.Setenv("PAD_DB_DRIVER", "")
	t.Setenv("PAD_HOST", "127.0.0.1")
	t.Setenv("PAD_PORT", strconv.Itoa(port))
	t.Setenv("PAD_LOG_LEVEL", "error")

	cmd := serveCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("start succeeded against a held port")
	}

	// The control that this is the losing-start path and not a config that
	// pointed somewhere else: the database path is the one the start would
	// have opened, and the port is the held one.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("losing start touched the database at %s (stat err %v): it must exit before opening it", dbPath, err)
	}
	for _, name := range []string{"pad.db-wal", "pad.db-shm", "pad.pid"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Fatalf("losing start left %s behind (stat err %v)", name, err)
		}
	}
}
