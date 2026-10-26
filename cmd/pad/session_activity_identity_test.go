package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSessionActivityChildExplicitIdentity(t *testing.T) {
	preserveRootFlagGlobals(t)
	record := sessionActivityRegistry(t)
	t.Chdir(t.TempDir())
	oldResolve := sessionActivityResolveIdentity
	t.Cleanup(func() { sessionActivityResolveIdentity = oldResolve })
	sessionActivityResolveIdentity = func() (string, string) {
		t.Error("child tried to resolve identity")
		return "", ""
	}
	// A registry row without a worker would be minted into if the child
	// consulted it. Assert below that it is left byte-for-byte unchanged.
	registryWorker := record.WorkerID
	record.WorkerID = ""
	registryPath := filepath.Join(os.Getenv("HOME"), ".pad", "sessions", strconv.Itoa(record.PID)+".json")
	registryBytes, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registryBytes, 0600); err != nil {
		t.Fatal(err)
	}
	// Both the registry and environment disagree with the captured identity.
	t.Setenv("PAD_AGENT", "environment-agent")
	t.Setenv("PAD_WORKER_ID", registryWorker)
	for _, tc := range []struct {
		name, agent, worker, wantWorker string
	}{
		{"captured", "Captured/Agent", "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{"empty", "", "", ""},
		{"invalid_worker", "Captured/Agent", "wk_invalid", ""},
		{"no_registry_lookup", "Captured/Agent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "no_registry_lookup" {
				t.Setenv("PAD_WORKER_ID", "")
			}
			arrived := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- r.Header.Clone()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			root := newRootCmd()
			activity, _, _ := root.Find([]string{"session", "activity"})
			for _, name := range []string{"detached-agent", "detached-worker"} {
				if flag := activity.Flags().Lookup(name); flag == nil || !flag.Hidden {
					t.Fatalf("%s must be hidden", name)
				}
			}
			root.SetArgs([]string{"--url", srv.URL, "session", "activity", "busy",
				"--detached-at", "2026-10-23T00:00:00Z", "--detached-agent", tc.agent, "--detached-worker", tc.worker})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			select {
			case headers := <-arrived:
				if got := headers.Get("X-Pad-Agent"); got != tc.agent {
					t.Errorf("X-Pad-Agent = %q, want captured %q", got, tc.agent)
				}
				if got := headers.Get("X-Pad-Worker"); got != tc.wantWorker {
					t.Errorf("X-Pad-Worker = %q, want captured %q", got, tc.wantWorker)
				}
			case <-time.After(time.Second):
				t.Fatal("child did not send activity with explicit identity")
			}
			if got, err := os.ReadFile(registryPath); err != nil || !bytes.Equal(got, registryBytes) {
				t.Fatalf("child changed registry: %v", err)
			}
		})
	}
}
