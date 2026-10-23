package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSessionActivityRealBinaryReturnsBeforeResponse(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pad")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Build before isolating HOME: the nested build uses the run's Go cache.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_WORKER_ID", "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("PAD_TOKEN", "activity-local-test")
	arrived := make(chan map[string]string, 1)
	finished := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		arrived <- body
		<-release
		w.WriteHeader(http.StatusNoContent)
		close(finished)
	}))
	defer srv.Close()
	defer close(release)
	cmd := exec.Command(bin, "--url", srv.URL, "session", "activity", "busy")
	cmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("parent: %v, stderr=%q", err, &stderr)
	}
	if elapsed := time.Since(start); elapsed >= 300*time.Millisecond {
		t.Errorf("parent waited %s for blocked child; want <300ms", elapsed)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("parent output: %q %q", &stdout, &stderr)
	}
	select {
	case body := <-arrived:
		at, err := time.Parse(time.RFC3339Nano, body["at"])
		if body["state"] != "busy" || err != nil || at.Before(start) || at.After(time.Now()) {
			t.Fatalf("request: %v, timestamp error: %v", body, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background request never arrived")
	}
	select {
	case <-finished:
		t.Fatal("child finished before parent returned")
	default:
	}
}
