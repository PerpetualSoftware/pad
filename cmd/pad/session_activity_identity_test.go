package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionActivityChildExplicitIdentity(t *testing.T) {
	record := sessionActivityRegistry(t)
	t.Chdir(t.TempDir())
	// Both the registry and environment disagree with the captured identity.
	t.Setenv("PAD_AGENT", "environment-agent")
	t.Setenv("PAD_WORKER_ID", record.WorkerID)
	for _, tc := range []struct {
		name, agent, worker, wantWorker string
	}{
		{"captured", "Captured/Agent", "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "wk_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{"empty", "", "", ""},
		{"invalid_worker", "Captured/Agent", "wk_invalid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrived := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- r.Header.Clone()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			root := newRootCmd()
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
		})
	}
}
