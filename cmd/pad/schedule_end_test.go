package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TASK-4537: the last occurrence of an open item.

const endedView = `{"id":"s1","item_ref":"TASK-5","roles":{},"dtstart":"2026-11-09T09:00:00","tz":"UTC","floating":false,"anchor":"calendar","audience":"both","ended_at":"2026-11-09T15:00:00Z"}`

func stillOpenReply(options string, ack bool) (int, string) {
	a := "false"
	if ack {
		a = "true"
	}
	return 409, `{"error":{"code":"item_still_open","message":"This is the schedule's last occurrence and the item is still open.","details":{"state":"done","occurrence_at":"2026-11-09T14:00:00Z","done_field":"status","item_status":"open","close_options":` + options + `,"ack_available":` + a + `}}}`
}

func endedResolved(stillOpen bool) string {
	extra := ""
	if stillOpen {
		extra = `,"item_still_open":true`
	}
	return `{"commitment":` + endedView + `,"resolution":{"occurrence_at":"2026-11-09T14:00:00Z","state":"done","missed_recorded":0` + extra + `}}`
}

func resolveBody(t *testing.T, c recordedCall) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal([]byte(c.body), &b); err != nil {
		t.Fatalf("body %q: %v", c.body, err)
	}
	return b
}

// endFlow answers the resolve with item_still_open until end_schedule is sent.
func endFlow(options string, patchStatus int) func(recordedCall) (int, string) {
	return func(c recordedCall) (int, string) {
		switch {
		case c.method == "PATCH":
			if patchStatus != 200 {
				return patchStatus, `{"error":{"code":"open_children","message":"TASK-5 has open children"}}`
			}
			return 200, `{"id":"i1","slug":"TASK-5"}`
		case strings.HasSuffix(c.path, "/resolve") && strings.Contains(c.body, `"end_schedule":true`):
			return 200, endedResolved(true)
		case strings.HasSuffix(c.path, "/resolve"):
			return stillOpenReply(options, false)
		}
		return 200, weekdaysView
	}
}

func TestRemindDoneCloseItemFlow(t *testing.T) {
	t.Run("retry then close", func(t *testing.T) {
		out, _, err, calls := runSched(t, endFlow(`["done"]`, 200), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--close-item")
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 3 || calls[0].path != schedPath+"/resolve" || calls[1].path != schedPath+"/resolve" || calls[2].method != "PATCH" {
			t.Fatalf("calls=%+v", calls)
		}
		if b := resolveBody(t, calls[0]); b["end_schedule"] != nil {
			t.Fatalf("first resolve consented: %v", b)
		}
		if b := resolveBody(t, calls[1]); b["end_schedule"] != true {
			t.Fatalf("retry = %v", b)
		}
		patch := resolveBody(t, calls[2])
		if fp, _ := patch["fields_patch"].(map[string]any); fp["status"] != "done" {
			t.Fatalf("patch = %v", patch)
		}
		if !strings.Contains(out, "the schedule has ended; closed TASK-5 (done)") {
			t.Fatalf("out=%q", out)
		}
	})
	t.Run("named value", func(t *testing.T) {
		_, _, err, calls := runSched(t, endFlow(`["shipped","done"]`, 200), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--close-item=Shipped")
		if err != nil || len(calls) != 3 {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
		if fp, _ := resolveBody(t, calls[2])["fields_patch"].(map[string]any); fp["status"] != "shipped" {
			t.Fatalf("patch = %s", calls[2].body)
		}
	})
	for _, tc := range []struct {
		name, options, flag string
		wants               []string
	}{
		{"ambiguous options write nothing", `["shipped","done"]`, "--close-item", []string{"shipped, done", "--close-item=VALUE"}},
		{"no option", `[]`, "--close-item", []string{"no delivering close value", "--end-schedule", "pad item update"}},
		{"explicit value without options", `[]`, "--close-item=cancelled", []string{"no delivering close value", "--end-schedule", "pad item update"}},
		{"value outside the options", `["done"]`, "--close-item=cancelled", []string{"not a delivering close", "done"}},
		{"value outside several options", `["shipped","done"]`, "--close-item=cancelled", []string{"not a delivering close", "shipped, done"}},
		{"explicit empty is a value", `["done"]`, "--close-item=", []string{"not a delivering close", "done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err, calls := runSched(t, endFlow(tc.options, 200), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", tc.flag)
			if err == nil {
				t.Fatal("expected refusal")
			}
			for _, want := range append(tc.wants, "Nothing was written") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if len(calls) != 1 || calls[0].method != "POST" || calls[0].path != schedPath+"/resolve" {
				t.Fatalf("wrote after the refusal: %+v", calls)
			}
			var marker strings.Builder
			writeRootStructuredError(&marker, err)
			if !strings.Contains(marker.String(), `"code":"item_still_open"`) {
				t.Errorf("lost envelope: %s", marker.String())
			}
		})
	}

	t.Run("PATCH failure", func(t *testing.T) {
		_, _, err, calls := runSched(t, endFlow(`["done"]`, 409), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--close-item")
		if err == nil || !strings.Contains(err.Error(), "Marked done TASK-5; the schedule has ended. Closing the item failed:") || !strings.Contains(err.Error(), "It stays open in Outstanding.") {
			t.Fatalf("err=%v", err)
		}
		if len(calls) != 3 {
			t.Fatalf("calls=%+v", calls)
		}
	})
	for _, format := range []string{"table", "json"} {
		t.Run("not the last occurrence/"+format, func(t *testing.T) {
			_, stderr, err, calls := runSched(t, okReply(resolved), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--close-item", "--format", format)
			if err != nil || len(calls) != 1 {
				t.Fatalf("err=%v calls=%+v", err, calls)
			}
			if stderr != "TASK-5 was not closed because the schedule continues.\n" {
				t.Fatalf("stderr=%q", stderr)
			}
		})
	}
}

func TestRemindEndScheduleFlags(t *testing.T) {
	for _, verb := range []string{"done", "skip"} {
		out, _, err, calls := runSched(t, okReply(endedResolved(true)), "", "remind", verb, "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--end-schedule")
		if err != nil || len(calls) != 1 {
			t.Fatalf("%s err=%v calls=%+v", verb, err, calls)
		}
		if b := resolveBody(t, calls[0]); b["end_schedule"] != true {
			t.Fatalf("%s body = %v", verb, b)
		}
		if !strings.Contains(out, "TASK-5 stays open in Outstanding until it is closed or dismissed (pad remind dismiss TASK-5).") {
			t.Fatalf("%s out=%q", verb, out)
		}
	}
	_, _, err, calls := runSched(t, okReply(resolved), "", "remind", "done", "TASK-5", "--end-schedule", "--close-item")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") || len(calls) != 0 {
		t.Fatalf("both flags err=%v calls=%v", err, calls)
	}
	for _, args := range [][]string{{"skip", "--close-item"}, {"snooze", "--until", "2h", "--end-schedule"}, {"move", "--until", "2026-11-10T09:00", "--end-schedule"}, {"ack", "--end-schedule"}} {
		_, _, err, calls := runSched(t, okReply(resolved), "", append([]string{"remind", args[0], "TASK-5"}, args[1:]...)...)
		if err == nil || len(calls) != 0 {
			t.Fatalf("%v err=%v calls=%v", args, err, calls)
		}
	}
}

func TestRemindItemStillOpenHints(t *testing.T) {
	for _, tc := range []struct{ name, options, closeHint string }{
		{"no options", `[]`, ""},
		{"one option", `["done"]`, "--close-item to close it too"},
		{"several options", `["shipped","done"]`, "--close-item=VALUE to close it too (choose one of: shipped, done)"},
	} {
		for _, state := range []string{"done", "skip"} {
			for _, ack := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/ack=%v", tc.name, state, ack), func(t *testing.T) {
					_, _, err, calls := runSched(t, func(recordedCall) (int, string) { return stillOpenReply(tc.options, ack) }, "", "remind", state, "TASK-5", "--occurrence", "2026-11-09T14:00:00Z")
					if err == nil || len(calls) != 1 {
						t.Fatalf("err=%v calls=%+v", err, calls)
					}
					if !strings.Contains(err.Error(), "--end-schedule to keep it open") || strings.Contains(err.Error(), "pad remind ack") != ack {
						t.Errorf("hint=%v", err)
					}
					if tc.closeHint == "" || state == "skip" {
						if strings.Contains(err.Error(), "--close-item") {
							t.Errorf("unavailable close suggested: %v", err)
						}
					} else if !strings.Contains(err.Error(), tc.closeHint) {
						t.Errorf("hint=%v, want %q", err, tc.closeHint)
					}
					var marker strings.Builder
					writeRootStructuredError(&marker, err)
					if !strings.Contains(marker.String(), `"code":"item_still_open"`) {
						t.Errorf("lost envelope: %s", marker.String())
					}
				})
			}
		}
	}
}

func TestRemindDismiss(t *testing.T) {
	for _, alias := range [][]string{{"remind"}, {"item", "remind"}} {
		out, _, err, calls := runSched(t, okReply(`{"commitment":`+endedView+`}`), "", append(alias, "dismiss", "TASK-5")...)
		if err != nil || len(calls) != 1 || calls[0].method != "POST" || calls[0].path != schedPath+"/dismiss-ended" {
			t.Fatalf("err=%v calls=%+v", err, calls)
		}
		if out != "Dismissed the ended schedule of TASK-5; the item stays open.\n" {
			t.Fatalf("out=%q", out)
		}
	}
	_, _, err, _ := runSched(t, apiError(409, "not_ended", "The schedule has an outstanding occurrence; there is nothing ended to dismiss.", ""), "", "remind", "dismiss", "TASK-5")
	if err == nil || !strings.Contains(err.Error(), "nothing ended to dismiss") {
		t.Fatalf("not_ended err=%v", err)
	}
	var marker strings.Builder
	writeRootStructuredError(&marker, err)
	if !strings.Contains(marker.String(), `"code":"not_ended"`) {
		t.Fatalf("lost envelope: %s", marker.String())
	}
}

func TestRemindDoneCloseItemJSON(t *testing.T) {
	out, _, err, calls := runSched(t, endFlow(`["done"]`, 200), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--close-item", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[2].method != "PATCH" {
		t.Fatalf("calls=%+v", calls)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	resolution, _ := body["resolution"].(map[string]any)
	closed, _ := body["item_closed"].(map[string]any)
	commitment, _ := body["commitment"].(map[string]any)
	if resolution["item_still_open"] == true || resolution["state"] != "done" || commitment["ended_at"] == nil || closed["field"] != "status" || closed["value"] != "done" {
		t.Fatalf("JSON does not describe the completed close: %s", out)
	}
}

func TestRemindDoneCloseItemWithoutClosingValue(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run("dismissed ending/"+format, func(t *testing.T) {
			body := strings.Replace(endedResolved(false), `"ended_at":`, `"ended_dismissed_at":"2026-11-09T15:00:00Z","ended_at":`, 1)
			out, stderr, err, calls := runSched(t, okReply(body), "", "remind", "done", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--close-item", "--format", format)
			if err != nil || len(calls) != 1 {
				t.Fatalf("err=%v calls=%+v", err, calls)
			}
			if stderr != "TASK-5 was not closed because its collection has no closing value.\n" {
				t.Errorf("stderr=%q", stderr)
			}
			if strings.Contains(out, "item_closed") || strings.Contains(out, "stays open in Outstanding") {
				t.Errorf("misleading output: %s", out)
			}
		})
	}
}
