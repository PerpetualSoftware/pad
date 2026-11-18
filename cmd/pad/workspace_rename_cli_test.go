package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkspaceRenameCLI(t *testing.T) {
	var method, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"id1","slug":"new","name":"New name"}`))
	}))
	defer srv.Close()
	setTempHomeMain(t)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	t.Chdir(t.TempDir())
	oldURL, oldWS, oldFormat := urlFlag, workspaceFlag, formatFlag
	t.Cleanup(func() { urlFlag, workspaceFlag, formatFlag = oldURL, oldWS, oldFormat })
	urlFlag, workspaceFlag, formatFlag = srv.URL, "unrelated-default", "json"
	cmd := workspaceRenameCmd()
	cmd.SetArgs([]string{"old", "new", "--name", "New name"})
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || path != "/api/v1/workspaces/old/slug" || body["expected_slug"] != "old" || body["slug"] != "new" || body["name"] != "New name" {
		t.Fatalf("request: %s %s %v", method, path, body)
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(out), &response); err != nil || response["slug"] != "new" {
		t.Fatalf("JSON output: %q, %v", out, err)
	}
}
