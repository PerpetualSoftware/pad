package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSessionActivityHookProcess(t *testing.T) {
	if os.Getenv("PAD_TEST_ACTIVITY_HOOK") != "1" {
		return
	}
	root := newRootCmd()
	root.SetArgs([]string{"--url", os.Getenv("PAD_TEST_ACTIVITY_URL"), "session", "activity", "busy"})
	if root.Execute() != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

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
	t.Setenv("PAD_TEST_ACTIVITY_HOOK", "1")
	t.Setenv("PAD_TEST_ACTIVITY_URL", "http://"+addr)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// A subprocess catches os.Exit, prompts, and writes to real stdout/stderr.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionActivityHookProcess$")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook must exit 0: %v; stderr: %s", err, &stderr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Count(stderr.String(), "\n") > 1 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
