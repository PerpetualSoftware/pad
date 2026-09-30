package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// BUG-3200: remote /mcp answered a move refused for a value only the caller
// can supply with the generic "Adjust the input shape and retry", and dropped
// the details that name the field. The code and shape stay validation_failed;
// the hint now names the `field` parameter that supplies it.
func moveRefusalEnvelope(t *testing.T, body map[string]any) ErrorEnvelope {
	t.Helper()
	b, err := json.Marshal(map[string]any{"error": body})
	if err != nil {
		t.Fatal(err)
	}
	res := classifyHTTPStatusKind(context.Background(), "item move", "/api/v1/workspaces/ws/items/TASK-1/move",
		http.StatusBadRequest, b, nil, ResourceItem, "TASK-1")
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	return env
}

func TestRemoteMoveNeedsValueHint(t *testing.T) {
	t.Run("missing_required_fields", func(t *testing.T) {
		env := moveRefusalEnvelope(t, map[string]any{
			"code": "missing_required_fields", "message": `Required fields missing: required field "severity" has no value`,
		})
		if env.Error.Code != ErrValidationFailed {
			t.Fatalf("code = %q, want validation_failed unchanged", env.Error.Code)
		}
		for _, want := range []string{`"severity" has no value`, `field: ["<key>=<value>"]`} {
			if !strings.Contains(env.Error.Hint, want) {
				t.Errorf("hint lacks %q: %s", want, env.Error.Hint)
			}
		}
	})
	t.Run("state_change_requires_value names the field and its values", func(t *testing.T) {
		env := moveRefusalEnvelope(t, map[string]any{
			"code": "state_change_requires_value", "message": "the move would reopen a done item",
			"details": map[string]any{"field": "status", "options": []string{"done", "archived"}, "from": "done", "to": "open"},
		})
		if env.Error.Code != ErrValidationFailed {
			t.Fatalf("code = %q, want validation_failed unchanged", env.Error.Code)
		}
		if !strings.Contains(env.Error.Hint, `field: ["status=<done|archived>"]`) {
			t.Errorf("hint: %s", env.Error.Hint)
		}
	})
	t.Run("CONTROL: any other 400 keeps the generic hint", func(t *testing.T) {
		env := moveRefusalEnvelope(t, map[string]any{"code": "malformed_override", "message": "no such field"})
		if !strings.Contains(env.Error.Hint, "Adjust the input shape and retry") || strings.Contains(env.Error.Hint, "field: [") {
			t.Errorf("hint: %s", env.Error.Hint)
		}
	})
}
