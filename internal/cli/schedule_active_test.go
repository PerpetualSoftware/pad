package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteScheduleActiveError(t *testing.T) {
	for _, next := range []string{`"2099-01-01T09:00:00Z"`, `null`} {
		e := &APIError{Code: "schedule_active", Message: "This item has a recurring schedule.", Details: json.RawMessage(`{"schedule_id":"s1","next_at":` + next + `}`)}
		var b bytes.Buffer
		WriteScheduleActiveError(&b, e)
		lines := strings.Split(b.String(), "\n")
		if !strings.HasPrefix(lines[0], StructuredErrorMarker) {
			t.Fatalf("missing marker: %s", b.String())
		}
		var got struct {
			Error struct {
				Code    string          `json:"code"`
				Details json.RawMessage `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], StructuredErrorMarker)), &got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Code != e.Code || string(got.Error.Details) != string(e.Details) {
			t.Fatalf("payload changed: %s", b.String())
		}
		if !strings.Contains(b.String(), "--force") || !strings.Contains(b.String(), "Next occurrence:") {
			t.Fatalf("missing recovery: %s", b.String())
		}
		if next != "null" && !strings.Contains(b.String(), "2099-01-01T09:00:00Z") {
			t.Fatalf("missing occurrence: %s", b.String())
		}
	}
}

func TestWriteOpenChildrenErrorScheduleActive(t *testing.T) {
	for _, next := range []string{`"2099-01-01T09:00:00Z"`, `null`, "absent"} {
		t.Run(next, func(t *testing.T) {
			details := `{"open_children":[{"ref":"TASK-2","title":"Child","status":"open","collection_slug":"tasks"}],"hidden_blocker_count":0,"done_field":"status","attempted_value":"done"`
			if next != "absent" {
				details += `,"schedule_active":{"schedule_id":"s1","next_at":` + next + `}`
			}
			details += `}`
			e := &APIError{Code: "open_children", Message: "Children still open", Details: json.RawMessage(details)}
			var b bytes.Buffer
			WriteOpenChildrenError(&b, e, e.AsOpenChildren())
			lines := strings.SplitN(b.String(), "\n", 2)
			var envelope struct {
				Error struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], StructuredErrorMarker)), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "open_children" {
				t.Fatalf("code: %s", b.String())
			}
			if !strings.Contains(lines[1], "TASK-2") || !strings.Contains(lines[1], "--force") {
				t.Fatalf("missing children/retry: %s", b.String())
			}
			if next == "absent" {
				if _, ok := envelope.Error.Details["schedule_active"]; ok {
					t.Fatal("unexpected schedule details")
				}
				if strings.Contains(lines[1], "recurring schedule") || strings.Contains(lines[1], "Next occurrence") {
					t.Fatalf("unexpected warning: %s", b.String())
				}
				return
			}
			active, ok := envelope.Error.Details["schedule_active"].(map[string]any)
			if !ok || active["schedule_id"] != "s1" {
				t.Errorf("lost schedule details: %s", b.String())
			}
			wantNext := "not currently set"
			if next != "null" {
				wantNext = "2099-01-01T09:00:00Z"
			}
			for _, want := range []string{"Next occurrence: " + wantNext, "recurring schedule", "Closing it stops its occurrences until"} {
				if !strings.Contains(lines[1], want) {
					t.Errorf("missing %q: %s", want, b.String())
				}
			}
		})
	}
}
