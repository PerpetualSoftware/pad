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

// TASK-3585: a keyed push retries once with the same key after a 5xx, and
// only against a server that advertises idempotency_keys.
func pushKeyServer(t *testing.T, honorsKeys bool, statuses ...int) (*Client, func() []string) {
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
		status := http.StatusOK
		if n < len(statuses) {
			status = statuses[n]
		}
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"error":{"code":"push_unconfirmed","message":"m"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ref":"TASK-1","workspace":"w","pushed":true,"message":"m"}`))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("HOME", t.TempDir())
	return NewClientFromURL(ts.URL), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), keys...) }
}

func TestTASK3585_PushRetriesOnceWithTheSameKey(t *testing.T) {
	c, keys := pushKeyServer(t, true, http.StatusBadGateway)
	if _, err := c.PushItemKeyed("w", "TASK-1", "m", "k-1"); err != nil {
		t.Fatalf("a 502 then a 200 must succeed: %v", err)
	}
	if got := keys(); len(got) != 2 || got[0] != "k-1" || got[1] != "k-1" {
		t.Fatalf("want two attempts with one key; got %v", got)
	}
}

func TestTASK3585_PushNoRetryWithoutTheCapabilityOrAKey(t *testing.T) {
	c, keys := pushKeyServer(t, false, http.StatusBadGateway)
	_, _ = c.PushItemKeyed("w", "TASK-1", "m", "k-2")
	if got := keys(); len(got) != 1 {
		t.Fatalf("retried a push against a server that ignores keys: %d attempts", len(got))
	}
	c2, keys2 := pushKeyServer(t, true, http.StatusBadGateway)
	_, _ = c2.PushItem("w", "TASK-1", "m")
	if got := keys2(); len(got) != 1 {
		t.Fatalf("retried a keyless push: %d attempts", len(got))
	}
}
