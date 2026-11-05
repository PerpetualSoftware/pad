package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TASK-4278: the database bound's 503 reads as a plain, retryable message with
// the server's wait, and no write is retried by the client on its own.
func busyServer(t *testing.T) (*Client, *int32) {
	t.Helper()
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_, _ = w.Write([]byte(`{"idempotency_keys":true}`))
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"database_busy","message":"The server is busy; nothing was saved. Please try again."}}`))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("HOME", t.TempDir())
	return NewClientFromURL(ts.URL), &hits
}

func TestTASK4278_DatabaseBusyMessageCarriesTheRetryAfterHint(t *testing.T) {
	c, _ := busyServer(t)
	err := c.get("/anything", nil)
	apiErr, ok := IsDatabaseBusy(err)
	if !ok {
		t.Fatalf("want a database_busy APIError, got %v", err)
	}
	if apiErr.Status != 503 || apiErr.RetryAfterSeconds != 2 {
		t.Fatalf("status/retry-after = %d/%d, want 503/2", apiErr.Status, apiErr.RetryAfterSeconds)
	}
	msg := err.Error()
	if !strings.Contains(msg, "nothing was saved") || !strings.Contains(msg, "retry in about 2s") {
		t.Fatalf("message lacks the plain wording or the hint: %q", msg)
	}
	for _, bad := range []string{"SQLSTATE", "pq:", "SELECT", "UPDATE "} {
		if strings.Contains(msg, bad) {
			t.Fatalf("message carries store text %q: %q", bad, msg)
		}
	}
}

func TestTASK4278_AnUnkeyedWriteIsNeverRetriedOnDatabaseBusy(t *testing.T) {
	c, hits := busyServer(t)
	if _, err := c.PushItem("w", "TASK-1", "m"); err == nil {
		t.Fatal("want the busy error")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("an unkeyed write was sent %d times, want exactly 1", got)
	}
}

func TestTASK4278_AKeyedWriteRetriesOnceWithItsKey(t *testing.T) {
	c, hits := busyServer(t)
	_, _ = c.PushItemKeyed("w", "TASK-1", "m", "k-1")
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("a keyed write was sent %d times, want 2 (the single keyed retry)", got)
	}
}
