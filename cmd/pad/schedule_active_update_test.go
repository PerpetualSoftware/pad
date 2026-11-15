package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// A bulk update keeps its {updated, failed, total} result when an item is
// refused with schedule_active: the refusal is a per-row failure, the command
// exits 0 (as it does for open_children rows), and no structured-error marker
// reaches stderr, so stdio MCP does not collapse the batch into one error.
func TestBulkUpdateScheduleActiveKeepsBatchResult(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			forced := false
			setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					forced = body["force"] == true
					if !forced {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error":{"code":"schedule_active","message":"Recurring schedule","details":{"schedule_id":"s1","next_at":"2099-01-01T09:00:00Z"}}}`))
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "item-1", "slug": "task", "title": "Task", "collection_slug": "tasks", "collection_prefix": "TASK", "item_number": 1, "fields": `{"status":"open"}`})
			}))
			oldFormat := formatFlag
			formatFlag = format
			t.Cleanup(func() { formatFlag = oldFormat })

			var execErr error
			var stdout string
			stderr := captureStderr(t, func() {
				stdout = captureStdout(t, func() {
					cmd := bulkUpdateCmd()
					cmd.SetArgs([]string{"--status", "done", "TASK-1"})
					cmd.SilenceUsage = true
					execErr = cmd.Execute()
				})
			})
			if execErr != nil {
				t.Fatalf("a per-row refusal must not fail the batch: %v", execErr)
			}
			if strings.Contains(stderr, cli.StructuredErrorMarker) {
				t.Fatalf("bulk refusal wrote the structured-error marker to stderr: %q", stderr)
			}
			if format == "json" {
				var res struct {
					Failed []struct {
						Code string `json:"code"`
					} `json:"failed"`
					Total int `json:"total"`
				}
				if err := json.Unmarshal([]byte(stdout), &res); err != nil {
					t.Fatalf("decode %q: %v", stdout, err)
				}
				if res.Total != 1 || len(res.Failed) != 1 || res.Failed[0].Code != "schedule_active" {
					t.Fatalf("want one schedule_active failure row, got %+v", res)
				}
			} else if !strings.Contains(stdout, "--force") {
				t.Fatalf("text output should name --force: %q", stdout)
			}

			cmd := bulkUpdateCmd()
			cmd.SetArgs([]string{"--status", "done", "--force", "TASK-1"})
			cmd.SilenceUsage = true
			_ = captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr != nil {
				t.Fatal(execErr)
			}
			if !forced {
				t.Fatal("force not sent")
			}
		})
	}
}
