package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestItemAckAmbiguityEmitsStructuredError(t *testing.T) {
	var out bytes.Buffer
	writeRootStructuredError(&out, &cli.APIError{Code: "ambiguous_reminder", Message: "2 fired reminders; ack by id"})
	if !strings.Contains(out.String(), cli.StructuredErrorMarker) || !strings.Contains(out.String(), `"code":"ambiguous_reminder"`) {
		t.Fatalf("missing ambiguity marker: %s", out.String())
	}
}

func TestItemAckRoutesUUIDAndRef(t *testing.T) {
	for _, tc := range []struct{ arg, path string }{
		{"67ec8261-505f-4ddc-8d40-fbb1393bb649", "/reminders/67ec8261-505f-4ddc-8d40-fbb1393bb649/ack"},
		{"TASK-9", "/items/TASK-9/reminders/ack"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			setTempHomeMain(t)
			var method, path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"67ec8261-505f-4ddc-8d40-fbb1393bb649","acked_at":"2026-11-12T12:00:00Z"}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("PAD_URL", srv.URL)
			t.Setenv("PAD_TOKEN", "pad_envtoken")
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, ".pad.toml"), []byte("workspace = \"ws1\"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			cmd := ackCmd()
			cmd.SetArgs([]string{tc.arg})
			captureStdout(t, func() { err = cmd.Execute() })
			if err != nil {
				t.Fatal(err)
			}
			if method != "POST" || path != "/api/v1/workspaces/ws1"+tc.path {
				t.Fatalf("got %s %s, want POST %s", method, path, tc.path)
			}
		})
	}
}
