package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemindReopen(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const result = `{"commitment":{},"reopen":{"resolution_id":"` + id + `","change_id":"22222222-2222-4222-8222-222222222222","previous_state":"done","missed_removed":0,"comment_id":"kept"}}`
	for _, alias := range [][]string{{"remind"}, {"item", "remind"}} {
		for _, explicit := range []bool{false, true} {
			for _, format := range []string{"table", "json"} {
				args := append(append([]string{}, alias...), "reopen", "TASK-5", "--format", format)
				if explicit {
					args = append(args, "--resolution", id)
				}
				out, _, err, calls := runSched(t, func(c recordedCall) (int, string) {
					if c.method == "GET" {
						return 200, `{"last_resolution":{"resolution_id":"` + id + `","can_reopen":true}}`
					}
					return 200, result
				}, "", args...)
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if explicit {
					want = 1
				}
				if len(calls) != want || calls[want-1].path != schedPath+"/reopen" {
					t.Fatalf("calls=%+v", calls)
				}
				var body map[string]any
				_ = json.Unmarshal([]byte(calls[want-1].body), &body)
				if len(body) != 1 || body["resolution_id"] != id {
					t.Fatalf("body=%v", body)
				}
				if format == "table" && (!strings.Contains(out, "undid done") || !strings.Contains(out, "comment was kept")) {
					t.Fatalf("text=%q", out)
				}
				if format == "json" && !strings.Contains(out, id) {
					t.Fatalf("JSON=%q", out)
				}
			}
		}
	}
	_, _, err, calls := runSched(t, func(c recordedCall) (int, string) {
		if c.method == "GET" {
			return 200, `{"last_resolution":{"resolution_id":"` + id + `"}}`
		}
		return 409, `{"error":{"code":"reopen_conflict","message":"changed"}}`
	}, "", "remind", "reopen", "TASK-5")
	if err == nil || len(calls) != 2 {
		t.Fatalf("conflict retried: %v %+v", err, calls)
	}
	var marker strings.Builder
	writeRootStructuredError(&marker, err)
	if !strings.Contains(marker.String(), `"code":"reopen_conflict"`) {
		t.Fatal(marker.String())
	}
	_, _, err, calls = runSched(t, okReply(`{}`), "", "remind", "reopen", "TASK-5")
	if err == nil || len(calls) != 1 {
		t.Fatalf("missing candidate: %v %+v", err, calls)
	}
}

func TestRemindReopenHistoryDone(t *testing.T) {
	out, _, err, _ := runSched(t, okReply(`{"commitment":{},"reopen":{"previous_state":"history_done","comment_id":"kept"}}`), "", "remind", "reopen", "TASK-5", "--resolution", "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Restored TASK-5’s lapsed occurrence; undid the late done. The resolution comment was kept.\n"; out != want {
		t.Fatalf("text=%q want %q", out, want)
	}
}

func TestRemindReopenDeletedComment(t *testing.T) {
	out, _, err, _ := runSched(t, okReply(`{"commitment":{},"reopen":{"previous_state":"done"}}`), "", "remind", "reopen", "TASK-5", "--resolution", "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "comment was kept") {
		t.Fatal(out)
	}
}
