package webhooks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// mockStore implements WebhookStore for testing.
type mockStore struct {
	mu       sync.Mutex
	hooks    []models.Webhook
	failures map[string]bool // id -> last failed state
	updated  chan string      // signals when UpdateWebhookFailure is called
}

func newMockStore(hooks []models.Webhook) *mockStore {
	return &mockStore{
		hooks:   hooks,
		updated: make(chan string, 10),
	}
}

func (m *mockStore) ListWebhooks(workspaceID string) ([]models.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []models.Webhook
	for _, h := range m.hooks {
		if h.WorkspaceID == workspaceID {
			result = append(result, h)
		}
	}
	return result, nil
}

func (m *mockStore) UpdateWebhookFailure(id string, failed bool) error {
	m.mu.Lock()
	if m.failures == nil {
		m.failures = make(map[string]bool)
	}
	m.failures[id] = failed
	m.mu.Unlock()
	m.updated <- id
	return nil
}

func (m *mockStore) waitForUpdate() {
	<-m.updated
}

func TestDispatcher_Dispatch(t *testing.T) {
	var received WebhookPayload
	var receivedSig string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		receivedSig = r.Header.Get("X-Pad-Signature")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Secret:      "test-secret",
			Events:      `["item.created", "item.updated"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test Item"})
	store.waitForUpdate()

	mu.Lock()
	defer mu.Unlock()
	if received.Event != "item.created" {
		t.Errorf("expected event 'item.created', got %q", received.Event)
	}
	if received.Workspace != "ws-1" {
		t.Errorf("expected workspace 'ws-1', got %q", received.Workspace)
	}
	if receivedSig == "" {
		t.Error("expected X-Pad-Signature header to be set")
	}
	store.mu.Lock()
	if store.failures["hook-1"] != false {
		t.Errorf("expected success (failed=false), got failed=true")
	}
	store.mu.Unlock()
}

func TestDispatcher_EventFiltering(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["item.created"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true

	// This event should NOT match — no goroutine launched, no store update
	d.Dispatch("ws-1", "item.deleted", map[string]string{"title": "Test"})

	// This event SHOULD match
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})
	store.waitForUpdate()

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("expected 1 delivery, got %d", callCount)
	}
}

func TestDispatcher_WildcardEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["*"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.deleted", map[string]string{"title": "Test"})
	store.waitForUpdate()

	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.failures["hook-1"]; !ok {
		t.Error("expected UpdateWebhookFailure to be called")
	}
	if store.failures["hook-1"] {
		t.Error("expected success, got failure")
	}
}

func TestDispatcher_InactiveWebhookSkipped(t *testing.T) {
	callCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["*"]`,
			Active:      false, // inactive
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})

	// Since the hook is inactive, no goroutine is launched
	if callCount != 0 {
		t.Errorf("expected 0 deliveries for inactive webhook, got %d", callCount)
	}
}

func TestDispatcher_FailureOnNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	store := newMockStore([]models.Webhook{
		{
			ID:          "hook-1",
			WorkspaceID: "ws-1",
			URL:         ts.URL,
			Events:      `["*"]`,
			Active:      true,
		},
	})

	d := NewDispatcher(store)
	d.SkipSSRF = true
	d.Dispatch("ws-1", "item.created", map[string]string{"title": "Test"})
	store.waitForUpdate()

	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.failures["hook-1"] {
		t.Error("expected failure to be recorded for non-2xx response")
	}
}

func TestMatchesEvent(t *testing.T) {
	tests := []struct {
		eventsJSON string
		event      string
		want       bool
	}{
		{`["*"]`, "item.created", true},
		{`["*"]`, "comment.created", true},
		{`["item.created"]`, "item.created", true},
		{`["item.created"]`, "item.deleted", false},
		{`["item.created", "item.updated"]`, "item.updated", true},
		{`["item.created", "item.updated"]`, "item.deleted", false},
		{`invalid`, "item.created", false},
		{`[]`, "item.created", false},
	}

	for _, tt := range tests {
		got := matchesEvent(tt.eventsJSON, tt.event)
		if got != tt.want {
			t.Errorf("matchesEvent(%q, %q) = %v, want %v", tt.eventsJSON, tt.event, got, tt.want)
		}
	}
}
