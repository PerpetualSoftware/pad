package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemindAckOccurrenceSelection(t *testing.T) {
	for _, alias := range [][]string{{"remind"}, {"item", "remind"}} {
		for _, explicit := range []bool{false, true} {
			args := append(append([]string{}, alias...), "ack", "TASK-5")
			if explicit {
				args = append(args, "--occurrence", "2026-11-09T14:00:00Z")
			}
			out, _, err, calls := runSched(t, func(c recordedCall) (int, string) {
				if c.method == "GET" {
					return 200, weekdaysView
				}
				return 200, resolved
			}, "", args...)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if explicit {
				want = 1
			}
			if len(calls) != want || calls[want-1].path != schedPath+"/resolve" {
				t.Fatalf("calls=%+v", calls)
			}
			var b map[string]any
			_ = json.Unmarshal([]byte(calls[want-1].body), &b)
			if b["state"] != "ack" || b["occurrence_at"] != "2026-11-09T14:00:00Z" || len(b) != 2 {
				t.Fatalf("body=%v", b)
			}
			if out != "Acknowledged TASK-5; this occurrence remains outstanding.\n" {
				t.Fatalf("out=%q", out)
			}
		}
	}
	for _, flag := range []string{"--until", "--at", "--comment", "-m"} {
		_, _, err, calls := runSched(t, okReply(resolved), "", "remind", "ack", "TASK-5", flag, "x")
		if err == nil || len(calls) != 0 {
			t.Fatalf("flag %s err=%v calls=%v", flag, err, calls)
		}
	}
	_, _, err, calls := runSched(t, apiError(409, "stale", "changed", `{"ended":true}`), "", "remind", "ack", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "changed") || len(calls) != 1 {
		t.Fatalf("stale=%v calls=%v", err, calls)
	}
	var marker strings.Builder
	writeRootStructuredError(&marker, err)
	if !strings.Contains(marker.String(), `"code":"stale"`) {
		t.Fatalf("lost stale envelope: %s", marker.String())
	}
}

func TestRemindAcknowledgedAgendaAttribution(t *testing.T) {
	for _, tc := range []struct{ name, actor, agent, want string }{{"Alex", "user", "", "acknowledged by Alex at "}, {"Alex", "agent", "rook", "acknowledged by Alex (rook via agent) at "}, {"", "user", "", "acknowledged by unknown user at "}} {
		body := `{"window_complete":true,"entries":[{"type":"schedule","at":"2026-11-09T14:00:00Z","state":"acknowledged","item_ref":"TASK-5","item_title":"Still owed","acknowledged_at":"2026-11-09T15:00:00Z","acknowledged_by":{"user_id":"u","name":"` + tc.name + `","actor":"` + tc.actor + `","agent_name":"` + tc.agent + `"}}]}`
		out, _, err, _ := runSched(t, okReply(body), "", "remind", "agenda", "--from", "2026-11-09", "--days", "1")
		if err != nil || !strings.Contains(out, "acknowledged") || !strings.Contains(out, tc.want) {
			t.Fatalf("agenda=%s err=%v", out, err)
		}
	}
	raw := strings.Replace(resolved, `"state":"done"`, `"state":"ack"`, 1)
	out, _, err, calls := runSched(t, okReply(raw), "", "remind", "ack", "TASK-5", "--occurrence", "2026-11-09T14:00:00Z", "--format", "json")
	if err != nil || len(calls) != 1 || !strings.Contains(out, `"state": "ack"`) {
		t.Fatalf("JSON=%s err=%v", out, err)
	}
}

// The attribution rides in the last column, so the rows after an acknowledged
// entry keep the table's alignment.
func TestRemindAcknowledgedAgendaKeepsColumns(t *testing.T) {
	body := `{"window_complete":true,"entries":[` +
		`{"type":"schedule","at":"2026-11-09T14:00:00Z","state":"acknowledged","item_ref":"TASK-5","item_title":"Still owed","acknowledged_at":"2026-11-09T15:00:00Z","acknowledged_by":{"user_id":"u","name":"Alex","actor":"user"}},` +
		`{"type":"schedule","at":"2026-11-10T14:00:00Z","state":"scheduled","item_ref":"TASK-5","item_title":"Still owed"}]}`
	out, _, err, _ := runSched(t, okReply(body), "", "remind", "agenda", "--from", "2026-11-09", "--days", "2", "--tz", "UTC")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var header, ack, next string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "DAY"):
			header = line
		case strings.Contains(line, "acknowledged by Alex"):
			ack = line
		case strings.Contains(line, "scheduled"):
			next = line
		}
	}
	if header == "" || ack == "" || next == "" {
		t.Fatalf("missing rows:\n%s", out)
	}
	if !strings.HasPrefix(ack, "2026-11-09") {
		t.Fatalf("attribution is not on the entry's own row:\n%s", out)
	}
	col := strings.Index(header, "REF")
	if strings.Index(ack, "TASK-5") != col || strings.Index(next, "TASK-5") != col {
		t.Fatalf("REF column misaligned (header at %d):\n%s", col, out)
	}
}

func TestAckTargetFallsBackToSlug(t *testing.T) {
	if got := ackTarget("TASK-5", "still-owed"); got != "TASK-5" {
		t.Fatalf("ref: %q", got)
	}
	if got := ackTarget("", "still-owed"); got != "still-owed" {
		t.Fatalf("slug fallback: %q", got)
	}
}
