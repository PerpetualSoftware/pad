package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetSessionActivity(t *testing.T) {
	const worker = "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/v1/sessions/activity" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Pad-Worker") != worker || r.Header.Get("Authorization") != "Bearer activity-test" {
					t.Error("missing worker or credential")
				}
				var body struct {
					State string `json:"state"`
					At    string `json:"at"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State != "busy" || body.At != "2026-10-23T00:00:00.123456789Z" {
					t.Errorf("body = %+v, %v", body, err)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := NewClientFromURL(server.URL)
			c.SetAuthToken("activity-test")
			if err := c.SetWorkerID(worker); err != nil {
				t.Fatal(err)
			}
			if err := c.SetSessionActivity(context.Background(), "busy", "2026-10-23T00:00:00.123456789Z"); (err != nil) != (status >= 400) {
				t.Fatalf("status %d: %v", status, err)
			}
		})
	}
}

func TestSetSessionActivityTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := NewClientFromURL(server.URL)
	if err := c.SetWorkerID("wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := c.SetSessionActivity(context.Background(), "idle", time.Now().UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("timeout took %v; want about 2s", elapsed)
	}
}
