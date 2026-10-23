package main

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSessionActivityUnreachable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", t.TempDir())
	t.Setenv("PAD_WORKER_ID", "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--url", "http://" + addr, "session", "activity", "busy"})
	start := time.Now()
	if err := root.Execute(); err != nil {
		t.Fatalf("hook must exit 0: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Count(stderr.String(), "\n") > 1 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("hook exceeded short timeout")
	}
}
