package mcp

// IDEA-1494 — confirm the HTTP dispatcher forwards the open-children
// guard override (`force`) into the PATCH body. Matches the wire
// contract handleUpdateItem reads (`force: true` at the top level of
// the ItemUpdate JSON body).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestDispatchItemUpdate_ForwardsForceFlag(t *testing.T) {
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/PLAN-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"PLAN-5","fields":"{\"status\":\"active\"}"}`))
		case http.MethodPatch:
			captured.ServeHTTP(w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"PLAN-5","status":"updated"}`))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp",
		"ref":       "PLAN-5",
		"status":    "completed",
		"force":     true,
	})
	res, err := d.Dispatch(ctx, []string{"item", "update"}, nil)
	if err != nil || (res != nil && res.IsError) {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if captured.requestCount != 1 {
		t.Fatalf("expected 1 PATCH, got %d", captured.requestCount)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, captured.lastBody)
	}
	got, ok := body["force"].(bool)
	if !ok || !got {
		t.Errorf("expected force=true in PATCH body, got %v (%T) — body=%s",
			body["force"], body["force"], captured.lastBody)
	}
}

func TestDispatchItemUpdate_OmitsForceWhenAbsent(t *testing.T) {
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/PLAN-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"PLAN-5","fields":"{\"status\":\"active\"}"}`))
		case http.MethodPatch:
			captured.ServeHTTP(w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"PLAN-5","status":"updated"}`))
		}
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp",
		"ref":       "PLAN-5",
		"status":    "completed",
	})
	if _, err := d.Dispatch(ctx, []string{"item", "update"}, nil); err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := body["force"]; present {
		t.Errorf("force should be omitted from PATCH body when not set, got body=%s", captured.lastBody)
	}
}
