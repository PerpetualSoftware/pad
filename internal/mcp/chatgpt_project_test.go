package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// Every ChatGPT tool has a response allow-list (TASK-3321 U3); a tool
// without one would fail closed at runtime, so it fails here first.
func TestChatGPTProjection_EveryToolHasAShape(t *testing.T) {
	for _, e := range ChatGPTCatalog {
		if _, ok := chatGPTResponseShapes[e.Name]; !ok {
			t.Errorf("%s has no response shape", e.Name)
		}
	}
	for name := range chatGPTResponseShapes {
		found := false
		for _, e := range ChatGPTCatalog {
			found = found || e.Name == name
		}
		if !found {
			t.Errorf("response shape for %s names no ChatGPT tool (stale)", name)
		}
	}
}

// An allow-list: a key the shape does not name is dropped, so a field added
// to an item tomorrow does not reach ChatGPT by default.
func TestChatGPTProjection_UnknownKeysAreDropped(t *testing.T) {
	in := map[string]any{"ref": "TASK-1", "title": "T", "a_new_internal_field": "x", "fields": map[string]any{"status": "open"}}
	got := itemShape.apply(in).(map[string]any)
	if _, leaked := got["a_new_internal_field"]; leaked {
		t.Errorf("an unlisted key passed the allow-list: %v", got)
	}
	if got["ref"] != "TASK-1" || got["fields"].(map[string]any)["status"] != "open" {
		t.Errorf("listed keys were not kept: %v", got)
	}
}

// A plan-limit refusal reaches ChatGPT without its upgrade text or billing
// link (OpenAI's no-upsell rule); other errors pass unchanged.
func TestChatGPTProjection_PlanLimitIsNeutral(t *testing.T) {
	upsell := NewErrorResult(ErrorPayload{
		Code:    ErrPlanLimitExceeded,
		Message: "You've reached the items limit on the free plan.",
		Hint:    "Upgrade to Pro at /console/billing to remove this limit.",
	})
	got := projectChatGPTResult(ChatGPTTool{Name: "create_item"}, nil, upsell)
	b, _ := json.Marshal(got)
	for _, banned := range []string{"Upgrade", "Pro", "/console/billing", "free plan"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("the ChatGPT plan-limit refusal still says %q: %s", banned, b)
		}
	}
	if !got.IsError || !strings.Contains(string(b), string(ErrPlanLimitExceeded)) {
		t.Errorf("the refusal lost its error code or flag: %s", b)
	}
}

// Other errors keep their code, message and hint, lose any other member,
// and have ids masked (codex review).
func TestChatGPTProjection_ErrorsAreMinimized(t *testing.T) {
	src := NewErrorResult(ErrorPayload{
		Code:    ErrNotFound,
		Message: "item 9b1c0a6e-1111-4222-8333-444455556666 not found",
		Hint:    "check the ref",
		Details: json.RawMessage(`{"assigned_user_email":"a@b.co"}`),
	})
	got := projectChatGPTResult(ChatGPTTool{Name: "get_item"}, nil, src)
	b, _ := json.Marshal(got)
	if !got.IsError || !strings.Contains(string(b), string(ErrNotFound)) || !strings.Contains(string(b), "check the ref") {
		t.Errorf("the error lost its code, flag or hint: %s", b)
	}
	if strings.Contains(string(b), "9b1c0a6e") || strings.Contains(string(b), "a@b.co") || strings.Contains(string(b), "details") {
		t.Errorf("the error still carries ids, emails or details: %s", b)
	}
}

// A success that is not JSON fails closed; archive_item, whose source
// answers no body, confirms without echoing an id.
func TestChatGPTProjection_NonJSONFailsClosed(t *testing.T) {
	if got := projectChatGPTResult(ChatGPTTool{Name: "get_item"}, nil, textResult("plain words")); !got.IsError {
		t.Errorf("a non-JSON success passed through: %+v", got)
	}
	got := projectChatGPTResult(ChatGPTTool{Name: "archive_item"},
		map[string]any{"ref": "9b1c0a6e-1111-4222-8333-444455556666"}, textResult(""))
	b, _ := json.Marshal(got.StructuredContent)
	if strings.Contains(string(b), "9b1c0a6e") || !strings.Contains(string(b), `"archived":true`) {
		t.Errorf("archive answer = %s, want archived:true without the id", b)
	}
}

// The text fallback is rewritten from the projected value, so a client that
// reads only text does not see what the structured content dropped.
func TestChatGPTProjection_TextFallbackIsProjected(t *testing.T) {
	raw := `{"ref":"TASK-1","id":"9b1c0a6e-1111-4222-8333-444455556666","assigned_user_email":"a@b.co"}`
	var v any
	_ = json.Unmarshal([]byte(raw), &v)
	got := projectChatGPTResult(ChatGPTTool{Name: "get_item"}, nil, structuredResult(v, raw))
	text := got.Content[0].(mcp.TextContent).Text
	if strings.Contains(text, "a@b.co") || strings.Contains(text, "9b1c0a6e") {
		t.Errorf("text fallback not projected: %s", text)
	}
}
