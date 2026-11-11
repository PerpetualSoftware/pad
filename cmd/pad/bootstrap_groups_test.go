package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBootstrapMarkdownGroupsAndCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"groups", `{"priority_list":[{"item_ref":"TASK-1","item_title":"Priority\nTitle","reason":"outstanding"}],"waiting_on":[{"item_ref":"TASK-2","item_title":"Waiting","reason":"waiting"}],"no_date":[{"item_ref":"TASK-3","item_title":"Unplaced","reason":"unplaced"}],"priority_list_more_count":2,"waiting_on_more_count":3,"no_date_more_count":4}`, []string{"Priority list (1):", "[TASK-1] Priority Title", "+2 more priority list", "Waiting on (1):", "[TASK-2] Waiting", "+3 more waiting on", "No date (1):", "[TASK-3] Unplaced", "+4 more no date"}},
		{"empty", `{}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// All completeness fields are present, as sent by the server.
			var a bootstrapAgendaView
			if err := json.Unmarshal([]byte(`{"due_today_complete":true,"carried_complete":true,"stale_complete":true,"priority_list_complete":true,"waiting_on_complete":true,"no_date_complete":true}`), &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.body), &a); err != nil {
				t.Fatal(err)
			}
			out := captureStdout(t, func() { printBootstrapAgenda(&a) })
			if tc.name == "empty" && out != "" {
				t.Fatalf("empty agenda printed: %q", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
		})
	}
	for _, flag := range []string{"due_today", "carried", "stale", "priority_list", "waiting_on", "no_date"} {
		t.Run(flag+" incomplete", func(t *testing.T) {
			var a bootstrapAgendaView
			blob := `{"due_today_complete":true,"carried_complete":true,"stale_complete":true,"priority_list_complete":true,"waiting_on_complete":true,"no_date_complete":true,"` + flag + `_complete":false}`
			if err := json.Unmarshal([]byte(blob), &a); err != nil {
				t.Fatal(err)
			}
			out := captureStdout(t, func() { printBootstrapAgenda(&a) })
			if !strings.Contains(out, "incomplete: "+strings.ReplaceAll(flag, "_", " ")) {
				t.Errorf("incompleteness hidden: %q", out)
			}
		})
	}
}
