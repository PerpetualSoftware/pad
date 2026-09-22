package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlaybookMatchCommand_TableOutput drives `pad playbook match` end to end
// against a fake HTTP server, covering both the CLI client method
// (MatchPlaybook) and the table-rendering RunE (PLAN-3114 unit 5, TASK-3120).
func TestPlaybookMatchCommand_TableOutput(t *testing.T) {
	var lastBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws/playbooks/match", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choice":     "PLAYB-5",
			"confidence": 0.83,
			"probabilities": map[string]float64{
				"PLAYB-5": 0.7,
				"none":    0.2,
				"PLAYB-9": 0.1,
			},
			"model": "jev-1.13.0",
			"options": []map[string]any{
				{"ref": "PLAYB-5", "title": "Ship a release", "invocation_slug": "ship"},
				{"ref": "PLAYB-9", "title": "Triage bugs"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")

	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", "table"

	cmd := playbookMatchCmd()
	cmd.SetArgs([]string{"ship these tasks"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("playbook match: %v", err)
		}
	})

	if lastBody["text"] != "ship these tasks" {
		t.Fatalf("server received text=%v, want \"ship these tasks\"", lastBody["text"])
	}
	if !strings.Contains(out, "PLAYB-5") || !strings.Contains(out, "Ship a release") {
		t.Errorf("output missing the matched playbook's ref/title:\n%s", out)
	}
	if !strings.Contains(out, "0.83") {
		t.Errorf("output missing the confidence:\n%s", out)
	}
	if !strings.Contains(out, "jev-1.13.0") {
		t.Errorf("output missing the model:\n%s", out)
	}
	if !strings.Contains(out, "PLAYB-9") {
		t.Errorf("output missing an unselected option's probability row:\n%s", out)
	}
}

// TestPlaybookMatchCommand_NoneAndJSON covers the "none" table rendering and
// the --format json passthrough in one pass.
func TestPlaybookMatchCommand_NoneAndJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws/playbooks/match", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choice":        "none",
			"confidence":    0.6,
			"probabilities": map[string]float64{"none": 0.6},
			"model":         "jev-1.13.0",
			"options":       []map[string]any{{"ref": "PLAYB-5", "title": "Ship a release"}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")

	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })

	workspaceFlag, formatFlag = "ws", "table"
	cmd := playbookMatchCmd()
	cmd.SetArgs([]string{"what's the weather"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("playbook match (table): %v", err)
		}
	})
	if !strings.Contains(out, "No matching playbook") {
		t.Errorf("table output for choice=none should say so plainly:\n%s", out)
	}

	workspaceFlag, formatFlag = "ws", "json"
	cmd = playbookMatchCmd()
	cmd.SetArgs([]string{"what's the weather"})
	out = captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("playbook match (json): %v", err)
		}
	})
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("--format json did not print valid JSON: %v\n%s", err, out)
	}
	if raw["choice"] != "none" {
		t.Errorf("json choice = %v, want none", raw["choice"])
	}
}
