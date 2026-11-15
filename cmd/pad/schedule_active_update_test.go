package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestBulkUpdateScheduleActiveExitAndForce(t *testing.T) {
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
			cmd := bulkUpdateCmd()
			cmd.SetArgs([]string{"--status", "done", "TASK-1"})
			cmd.SilenceUsage = true
			if err := cmd.Execute(); err == nil {
				t.Fatal("schedule refusal must exit non-zero")
			}
			cmd = bulkUpdateCmd()
			cmd.SetArgs([]string{"--status", "done", "--force", "TASK-1"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !forced {
				t.Fatal("force not sent")
			}
		})
	}
}
