package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
)

func testServerWithEvents(t *testing.T) *Server {
	t.Helper()
	srv := testServer(t)
	srv.SetEventBus(events.New())
	return srv
}

// sseEvent represents a parsed SSE event from the stream.
type sseEvent struct {
	Type string
	Data string
}

// connectSSE connects to the SSE endpoint on a real test server and returns
// a channel of parsed events. Cancel the context to disconnect.
func connectSSE(ctx context.Context, t *testing.T, baseURL, workspaceSlug string) <-chan sseEvent {
	t.Helper()
	ch := make(chan sseEvent, 32)

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/v1/events?workspace="+workspaceSlug, nil)
	if err != nil {
		t.Fatalf("failed to create SSE request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to connect to SSE: %v", err)
	}

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		var eventType, data string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if eventType != "" && data != "" {
					ch <- sseEvent{Type: eventType, Data: data}
					eventType = ""
					data = ""
				}
			}
		}
	}()

	return ch
}

// waitForEvent reads from the event channel with a timeout.
func waitForEvent(t *testing.T, ch <-chan sseEvent, timeout time.Duration) sseEvent {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return event
	case <-time.After(timeout):
		t.Fatal("timed out waiting for SSE event")
		return sseEvent{}
	}
}

// apiRequest makes a JSON API request to the test server.
func apiRequest(t *testing.T, baseURL, method, path string, body interface{}) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// createTestWorkspace creates a workspace via API and returns its slug.
func createTestWorkspace(t *testing.T, baseURL, name string) string {
	t.Helper()
	resp := apiRequest(t, baseURL, "POST", "/api/v1/workspaces", map[string]string{"name": name})
	defer resp.Body.Close()
	var ws models.Workspace
	json.NewDecoder(resp.Body).Decode(&ws)
	return ws.Slug
}

func TestSSEConnectedEvent(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectSSE(ctx, t, ts.URL, slug)
	event := waitForEvent(t, ch, 3*time.Second)

	if event.Type != "connected" {
		t.Errorf("expected 'connected' event, got %q", event.Type)
	}
	var data map[string]string
	json.Unmarshal([]byte(event.Data), &data)
	if data["workspace"] != slug {
		t.Errorf("expected workspace %q, got %q", slug, data["workspace"])
	}
}

func TestSSEDocumentCreatedEvent(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectSSE(ctx, t, ts.URL, slug)
	waitForEvent(t, ch, 3*time.Second) // connected

	// Create a document
	apiRequest(t, ts.URL, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title":    "SSE Test Doc",
		"content":  "Hello SSE",
		"doc_type": "notes",
	})

	event := waitForEvent(t, ch, 3*time.Second)
	if event.Type != events.DocumentCreated {
		t.Errorf("expected %q, got %q", events.DocumentCreated, event.Type)
	}
	var data events.Event
	json.Unmarshal([]byte(event.Data), &data)
	if data.Title != "SSE Test Doc" {
		t.Errorf("expected title 'SSE Test Doc', got %q", data.Title)
	}
	if data.DocType != "notes" {
		t.Errorf("expected doc_type 'notes', got %q", data.DocType)
	}
}

func TestSSEDocumentUpdatedEvent(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	// Create doc first
	resp := apiRequest(t, ts.URL, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title":   "Update Test",
		"content": "Original",
	})
	var doc models.Document
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectSSE(ctx, t, ts.URL, slug)
	waitForEvent(t, ch, 3*time.Second) // connected

	// Update document
	apiRequest(t, ts.URL, "PATCH", fmt.Sprintf("/api/v1/workspaces/%s/documents/%s", slug, doc.ID), map[string]interface{}{
		"content": "Updated",
	})

	event := waitForEvent(t, ch, 3*time.Second)
	if event.Type != events.DocumentUpdated {
		t.Errorf("expected %q, got %q", events.DocumentUpdated, event.Type)
	}
}

func TestSSEDocumentArchivedAndRestoredEvents(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	resp := apiRequest(t, ts.URL, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title": "Archive Test",
	})
	var doc models.Document
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectSSE(ctx, t, ts.URL, slug)
	waitForEvent(t, ch, 3*time.Second) // connected

	// Archive
	apiRequest(t, ts.URL, "DELETE", fmt.Sprintf("/api/v1/workspaces/%s/documents/%s", slug, doc.ID), nil)

	event := waitForEvent(t, ch, 3*time.Second)
	if event.Type != events.DocumentArchived {
		t.Errorf("expected %q, got %q", events.DocumentArchived, event.Type)
	}

	// Restore
	apiRequest(t, ts.URL, "POST", fmt.Sprintf("/api/v1/workspaces/%s/documents/%s/restore", slug, doc.ID), nil)

	event = waitForEvent(t, ch, 3*time.Second)
	if event.Type != events.DocumentRestored {
		t.Errorf("expected %q, got %q", events.DocumentRestored, event.Type)
	}
}

func TestSSEMultipleClients(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1 := connectSSE(ctx, t, ts.URL, slug)
	ch2 := connectSSE(ctx, t, ts.URL, slug)

	waitForEvent(t, ch1, 3*time.Second) // connected
	waitForEvent(t, ch2, 3*time.Second) // connected

	// Create doc
	apiRequest(t, ts.URL, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title": "Multi Client Test",
	})

	// Both clients should receive the event
	e1 := waitForEvent(t, ch1, 3*time.Second)
	e2 := waitForEvent(t, ch2, 3*time.Second)

	if e1.Type != events.DocumentCreated {
		t.Errorf("client 1: expected %q, got %q", events.DocumentCreated, e1.Type)
	}
	if e2.Type != events.DocumentCreated {
		t.Errorf("client 2: expected %q, got %q", events.DocumentCreated, e2.Type)
	}
}

func TestSSEWorkspaceIsolation(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug1 := createTestWorkspace(t, ts.URL, "WS One")
	slug2 := createTestWorkspace(t, ts.URL, "WS Two")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch2 := connectSSE(ctx, t, ts.URL, slug2)
	waitForEvent(t, ch2, 3*time.Second) // connected

	// Create doc in WS One
	apiRequest(t, ts.URL, "POST", "/api/v1/workspaces/"+slug1+"/documents", map[string]interface{}{
		"title": "WS1 Doc",
	})

	// WS Two subscriber should NOT get the event
	select {
	case event := <-ch2:
		t.Fatalf("ws2 should not receive ws1 events, got %q", event.Type)
	case <-time.After(300 * time.Millisecond):
		// Good
	}
}

func TestSSESubscriberCleanup(t *testing.T) {
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())

	ch := connectSSE(ctx, t, ts.URL, slug)
	waitForEvent(t, ch, 3*time.Second) // connected

	// Should have at least one subscriber
	if srv.events.SubscriberCount() < 1 {
		t.Fatalf("expected at least 1 subscriber, got %d", srv.events.SubscriberCount())
	}

	// Cancel context (simulates disconnect)
	cancel()

	// Wait for cleanup
	time.Sleep(300 * time.Millisecond)

	if srv.events.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers after disconnect, got %d", srv.events.SubscriberCount())
	}
}

// Error case tests use doRequest since they return immediately

func TestSSEMissingWorkspace(t *testing.T) {
	srv := testServerWithEvents(t)
	rr := doRequest(srv, "GET", "/api/v1/events?workspace=nonexistent", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestSSEMissingWorkspaceParam(t *testing.T) {
	srv := testServerWithEvents(t)
	rr := doRequest(srv, "GET", "/api/v1/events", nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestSSENoEventBus(t *testing.T) {
	srv := testServer(t) // no event bus
	rr := doRequest(srv, "GET", "/api/v1/events?workspace=test", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}

func TestSSEGlobalConnectionLimit(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.SetSSELimits(1, 0) // global limit of 1, no per-workspace limit
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First connection should succeed
	ch := connectSSE(ctx, t, ts.URL, slug)
	waitForEvent(t, ch, 3*time.Second) // connected

	// Second connection should be rejected with 429
	req, err := http.NewRequest("GET", ts.URL+"/api/v1/events?workspace="+slug, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", resp.StatusCode)
	}
}

func TestSSEPerWorkspaceLimit(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.SetSSELimits(0, 1) // no global limit, per-workspace limit of 1
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug1 := createTestWorkspace(t, ts.URL, "WS One")
	slug2 := createTestWorkspace(t, ts.URL, "WS Two")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First connection to ws1 should succeed
	ch1 := connectSSE(ctx, t, ts.URL, slug1)
	waitForEvent(t, ch1, 3*time.Second) // connected

	// Second connection to ws1 should be rejected
	req, err := http.NewRequest("GET", ts.URL+"/api/v1/events?workspace="+slug1, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 for second ws1 connection, got %d", resp.StatusCode)
	}

	// Connection to ws2 should still succeed (different workspace)
	ch2 := connectSSE(ctx, t, ts.URL, slug2)
	event := waitForEvent(t, ch2, 3*time.Second)
	if event.Type != "connected" {
		t.Errorf("expected 'connected' for ws2, got %q", event.Type)
	}
}

func TestSSELimitsExistingConnectionsUnaffected(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.SetSSELimits(1, 0) // global limit of 1
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Establish connection
	ch := connectSSE(ctx, t, ts.URL, slug)
	waitForEvent(t, ch, 3*time.Second) // connected

	// Try (and fail) to get a second connection
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events?workspace="+slug, nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// The existing connection should still work — publish an event
	srv.events.Publish(events.Event{
		Type:        "item.created",
		WorkspaceID: slug, // need the real workspace ID
	})

	// We can't easily test the existing connection receives events here
	// because the workspace ID in the event must match the internal UUID,
	// but we can verify the subscriber count is still 1
	if got := srv.events.SubscriberCount(); got != 1 {
		t.Errorf("expected 1 subscriber still active, got %d", got)
	}
}
