package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TASK-3585: the copy's one keyed retry. A server that advertises
// idempotency_keys gets a second attempt with the SAME key after an unknown
// outcome; one that does not, or a keyless call, gets exactly one attempt.
func copyKeyServer(t *testing.T, honorsKeys bool, statuses ...int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var keys []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"idempotency_keys": honorsKeys})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		k, _ := b["op_key"].(string)
		mu.Lock()
		n := len(keys)
		keys = append(keys, k)
		mu.Unlock()
		status := http.StatusCreated
		if n < len(statuses) {
			status = statuses[n]
		}
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"error":{"code":"copy_failed","message":"the copy may or may not have committed"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"destination":{"ref":"TASK-9","workspace_slug":"b","collection_slug":"tasks"}}`))
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), keys...) }
}

func TestTASK3585_CopyRetriesOnceWithTheSameKey(t *testing.T) {
	ts, keys := copyKeyServer(t, true, http.StatusInternalServerError)
	c := newCopyTestClient(t, ts)
	res, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks", OpKey: "k-1"})
	if err != nil || res == nil || res.Destination.Ref != "TASK-9" {
		t.Fatalf("a 500 then a 201 must succeed with the first copy's answer: %v", err)
	}
	if got := keys(); len(got) != 2 || got[0] != "k-1" || got[1] != "k-1" {
		t.Fatalf("want two attempts with one key; got %v", got)
	}
}

func TestTASK3585_CopyNoRetryWithoutTheCapability(t *testing.T) {
	ts, keys := copyKeyServer(t, false, http.StatusInternalServerError)
	c := newCopyTestClient(t, ts)
	if _, _, err := c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks", OpKey: "k-2"}); err == nil || !CopyOutcomeUnknown(err) {
		t.Fatalf("an unknown outcome must stay unknown: %v", err)
	}
	if got := keys(); len(got) != 1 {
		t.Fatalf("retried a copy against a server that ignores keys: %d attempts (a duplicate)", len(got))
	}
}

func TestTASK3585_CopyWithoutAKeyIsNeverRetried(t *testing.T) {
	ts, keys := copyKeyServer(t, true, http.StatusInternalServerError)
	c := newCopyTestClient(t, ts)
	_, _, _ = c.CopyItem("ws", "TASK-1", ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if got := keys(); len(got) != 1 {
		t.Fatalf("a keyless copy was retried: %d attempts", len(got))
	}
}
