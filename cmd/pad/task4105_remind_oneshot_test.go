package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCall struct{ method, path, query, body string }

// recordingStub answers every request through reply and records it.
func recordingStub(reply func(c recordedCall) (int, string)) (http.HandlerFunc, func() []recordedCall) {
	var mu sync.Mutex
	var calls []recordedCall
	h := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := recordedCall{r.Method, strings.TrimPrefix(r.URL.Path, "/api/v1"), r.URL.RawQuery, strings.TrimSpace(string(b))}
		if c.path == "/auth/me" {
			fmt.Fprint(w, `{"name":"Viewer","timezone":"America/New_York"}`)
			return
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		status, body := reply(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
	return h, func() []recordedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedCall(nil), calls...)
	}
}

// The plain reminder is pinned: its requests, its output and its errors.
func TestRemindOneShotUnchanged(t *testing.T) {
	reminder := `{"id":"r1","item_id":"i1","remind_at":"2099-01-01T09:00:00Z"}`
	reply := func(c recordedCall) (int, string) {
		switch {
		case c.method == "GET" && c.path == "/workspaces/test/items/TASK-5":
			return 200, `{"id":"i1","ref":"TASK-5","slug":"t-5","title":"T","collection_slug":"tasks","fields":"{}"}`
		case c.method == "POST" && c.path == "/workspaces/test/items/t-5/reminders":
			return 201, reminder
		case c.method == "PATCH" && c.path == "/workspaces/test/reminders/r1":
			return 200, reminder
		}
		return 404, `{"error":{"code":"not_found","message":"no"}}`
	}
	run := func(args ...string) (string, error, []recordedCall) {
		h, calls := recordingStub(reply)
		root := tzCLIWithLocal(t, h, time.UTC)
		out, _, err := tzRun(t, root, args...)
		return out, err, calls()
	}
	noSchedule := func(calls []recordedCall) {
		t.Helper()
		for _, c := range calls {
			if strings.Contains(c.path, "/schedule") {
				t.Fatalf("one-shot touched the schedule API: %+v", c)
			}
		}
	}

	t.Run("arm", func(t *testing.T) {
		out, err, calls := run("item", "remind", "TASK-5", "--remind-at", "2099-01-01T09:00:00Z")
		if err != nil || out != "Reminder armed on TASK-5 for 2099-01-01T09:00:00Z (id r1)\n" {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if len(calls) != 2 || calls[1].method != "POST" || calls[1].body != `{"remind_at":"2099-01-01T09:00:00Z"}` {
			t.Fatalf("calls=%+v", calls)
		}
		noSchedule(calls)
	})
	t.Run("rearm", func(t *testing.T) {
		out, err, calls := run("item", "remind", "--rearm", "r1", "--remind-at", "2099-01-01T09:00:00Z")
		if err != nil || out != "Re-armed reminder r1 for 2099-01-01T09:00:00Z\n" {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if len(calls) != 1 || calls[0].method != "PATCH" || calls[0].body != `{"remind_at":"2099-01-01T09:00:00Z"}` {
			t.Fatalf("calls=%+v", calls)
		}
		noSchedule(calls)
	})
	t.Run("errors", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"item", "remind", "TASK-5"}, "--remind-at is required (an RFC3339 instant, e.g. 2026-08-01T09:00:00Z)"},
			{[]string{"item", "remind", "--remind-at", "2099-01-01T09:00:00Z"}, "an item ref is required (e.g. pad item remind TASK-5 --remind-at 2026-08-01T09:00:00Z), or use --rearm <id> to move an existing reminder"},
			{[]string{"item", "remind", "TASK-5", "--rearm", "r1", "--remind-at", "2099-01-01T09:00:00Z"}, `--rearm addresses a reminder by id, so it takes no item ref (got "TASK-5")`},
		} {
			_, err, calls := run(tc.args...)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("args=%v err=%v want %q", tc.args, err, tc.want)
			}
			noSchedule(calls)
		}
	})
}
