package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestNativeItemAndNativeItems(t *testing.T) {
	t.Run("object fields and array tags with other keys unchanged", func(t *testing.T) {
		item := &models.Item{
			ID:          "11111111-1111-1111-1111-111111111111",
			WorkspaceID: "ws-1",
			Ref:         "TASK-5",
			Title:       "Native item",
			Slug:        "native-item",
			Seq:         7,
			Fields:      `{"status":"open","big":12345678901234567890}`,
			Tags:        `["a","b"]`,
		}

		b, err := json.Marshal(NativeItem(item))
		if err != nil {
			t.Fatalf("marshal NativeItem: %v", err)
		}
		if !strings.Contains(string(b), "12345678901234567890") {
			t.Fatalf("big number digits lost: %s", b)
		}

		var got map[string]any
		if err := models.DecodeJSONKeepingNumbers(b, &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		fields, ok := got["fields"].(map[string]any)
		if !ok {
			t.Fatalf("fields = %T (%v), want object", got["fields"], got["fields"])
		}
		if fields["status"] != "open" {
			t.Errorf("fields.status = %v, want open", fields["status"])
		}
		if big, ok := fields["big"].(json.Number); !ok || big.String() != "12345678901234567890" {
			t.Errorf("fields.big = %T(%v), want 12345678901234567890", fields["big"], fields["big"])
		}
		tags, ok := got["tags"].([]any)
		if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
			t.Errorf("tags = %T(%#v), want [a b]", got["tags"], got["tags"])
		}
		if got["workspace_id"] != "ws-1" || got["ref"] != "TASK-5" || got["title"] != "Native item" {
			t.Errorf("other keys changed: %v", got)
		}
	})

	t.Run("empty strings fall back to empty object and array", func(t *testing.T) {
		item := &models.Item{
			ID:     "2",
			Ref:    "TASK-6",
			Fields: "",
			Tags:   "   ",
		}
		b, err := json.Marshal(NativeItem(item))
		if err != nil {
			t.Fatalf("marshal NativeItem: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		fields, ok := got["fields"].(map[string]any)
		if !ok || len(fields) != 0 {
			t.Errorf("fields = %T(%v), want empty object {}", got["fields"], got["fields"])
		}
		tags, ok := got["tags"].([]any)
		if !ok || len(tags) != 0 {
			t.Errorf("tags = %T(%v), want empty array []", got["tags"], got["tags"])
		}
	})

	t.Run("invalid JSON string stays a JSON string", func(t *testing.T) {
		item := &models.Item{
			ID:     "3",
			Ref:    "TASK-7",
			Fields: "{not json",
			Tags:   "[broken",
		}
		b, err := json.Marshal(NativeItem(item))
		if err != nil {
			t.Fatalf("marshal NativeItem: %v", err)
		}
		if !json.Valid(b) {
			t.Fatalf("invalid JSON output: %s", b)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		if s, ok := got["fields"].(string); !ok || s != "{not json" {
			t.Errorf("fields = %T(%v), want string %q", got["fields"], got["fields"], "{not json")
		}
		if s, ok := got["tags"].(string); !ok || s != "[broken" {
			t.Errorf("tags = %T(%v), want string %q", got["tags"], got["tags"], "[broken")
		}
	})

	t.Run("nil item stays null", func(t *testing.T) {
		b, err := json.Marshal(NativeItem(nil))
		if err != nil {
			t.Fatalf("marshal NativeItem(nil): %v", err)
		}
		if string(b) != "null" {
			t.Errorf("NativeItem(nil) = %s, want null", b)
		}
	})

	t.Run("nil and empty slices stay empty array", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			items []models.Item
		}{
			{"nil slice", nil},
			{"empty slice", []models.Item{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b, err := json.Marshal(NativeItems(tc.items))
				if err != nil {
					t.Fatalf("marshal NativeItems: %v", err)
				}
				if string(b) != "[]" {
					t.Errorf("NativeItems = %s, want []", b)
				}
			})
		}
	})

	t.Run("multiple items preserve order and convert each", func(t *testing.T) {
		items := []models.Item{
			{ID: "1", Ref: "TASK-1", Fields: `{"status":"open"}`, Tags: `["x"]`},
			{ID: "2", Ref: "TASK-2", Fields: "", Tags: ""},
		}
		b, err := json.Marshal(NativeItems(items))
		if err != nil {
			t.Fatalf("marshal NativeItems: %v", err)
		}
		var got []map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, b)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if f0, ok := got[0]["fields"].(map[string]any); !ok || f0["status"] != "open" {
			t.Errorf("item[0].fields = %T(%v), want object {status:open}", got[0]["fields"], got[0]["fields"])
		}
		if t0, ok := got[0]["tags"].([]any); !ok || len(t0) != 1 || t0[0] != "x" {
			t.Errorf("item[0].tags = %T(%v), want [x]", got[0]["tags"], got[0]["tags"])
		}
		if f1, ok := got[1]["fields"].(map[string]any); !ok || len(f1) != 0 {
			t.Errorf("item[1].fields = %T(%v), want {}", got[1]["fields"], got[1]["fields"])
		}
		if t1, ok := got[1]["tags"].([]any); !ok || len(t1) != 0 {
			t.Errorf("item[1].tags = %T(%v), want []", got[1]["tags"], got[1]["tags"])
		}
	})
}

func TestRewriteRawItemJSON(t *testing.T) {
	t.Run("copy shape preserves key order, big integer, and unknown keys", func(t *testing.T) {
		raw := json.RawMessage(`{"z_unknown":{"k2":2,"k1":1},"huge":12345678901234567890,"item":{"z_first":true,"fields":"{\"z\":1,\"big\":12345678901234567890,\"a\":2}","tags":"[\"t1\",\"t2\"]","a_last":"ok"},"a_tail":9007199254740993}`)
		got := RewriteRawItemJSON(raw, "item")
		want := `{"z_unknown":{"k2":2,"k1":1},"huge":12345678901234567890,"item":{"z_first":true,"fields":{"z":1,"big":12345678901234567890,"a":2},"tags":["t1","t2"],"a_last":"ok"},"a_tail":9007199254740993}`
		if string(got) != want {
			t.Errorf("RewriteRawItemJSON mismatch:\n got: %s\nwant: %s", got, want)
		}

		var buf bytes.Buffer
		if err := PrintRawJSON(&buf, got); err != nil {
			t.Fatalf("PrintRawJSON: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "12345678901234567890") || !strings.Contains(out, "9007199254740993") {
			t.Errorf("PrintRawJSON lost big integer digits:\n%s", out)
		}
		if strings.Index(out, `"z_unknown"`) > strings.Index(out, `"a_tail"`) {
			t.Errorf("top-level key order not preserved:\n%s", out)
		}
		if strings.Index(out, `"z_first"`) > strings.Index(out, `"a_last"`) {
			t.Errorf("item key order not preserved:\n%s", out)
		}
		if strings.Index(out, `"z": 1`) > strings.Index(out, `"a": 2`) {
			t.Errorf("fields key order not preserved:\n%s", out)
		}
	})

	t.Run("search shape rewrites every results[].item and preserves rest", func(t *testing.T) {
		raw := json.RawMessage(`{"z_meta":12345678901234567890,"results":[{"snippet":"s1","item":{"ref":"TASK-1","fields":"{\"status\":\"open\"}","tags":"[\"a\"]"}},{"snippet":"s2","item":{"ref":"TASK-2","fields":"","tags":"  "}}],"a_extra":"keep"}`)
		got := RewriteRawItemJSON(raw, "results", "*", "item")
		want := `{"z_meta":12345678901234567890,"results":[{"snippet":"s1","item":{"ref":"TASK-1","fields":{"status":"open"},"tags":["a"]}},{"snippet":"s2","item":{"ref":"TASK-2","fields":{},"tags":[]}}],"a_extra":"keep"}`
		if string(got) != want {
			t.Errorf("RewriteRawItemJSON search mismatch:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("already-native fields and tags are left untouched", func(t *testing.T) {
		raw := json.RawMessage(`{"item":{"fields":{"z":1,"a":2},"tags":["x"],"other":12345678901234567890}}`)
		got := RewriteRawItemJSON(raw, "item")
		if string(got) != string(raw) {
			t.Errorf("already-native input was modified:\n got: %s\nwant: %s", got, raw)
		}
	})

	t.Run("invalid JSON inside fields string stays a JSON string", func(t *testing.T) {
		raw := json.RawMessage(`{"item":{"fields":"{not json","tags":"[\"ok\"]"}}`)
		got := RewriteRawItemJSON(raw, "item")
		want := `{"item":{"fields":"{not json","tags":["ok"]}}`
		if string(got) != want {
			t.Errorf("invalid fields string:\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("invalid JSON document passes through unchanged", func(t *testing.T) {
		raw := json.RawMessage(`{"item": {not valid json`)
		got := RewriteRawItemJSON(raw, "item")
		if string(got) != string(raw) {
			t.Errorf("invalid JSON modified:\n got: %q\nwant: %q", got, raw)
		}
	})

	t.Run("missing item key or unexpected shape passes through unchanged", func(t *testing.T) {
		cases := []struct {
			name string
			path []string
			raw  string
		}{
			{"missing item key", []string{"item"}, `{"ok":true,"big":12345678901234567890}`},
			{"item is not an object", []string{"item"}, `{"item":"not-an-object"}`},
			{"item is null", []string{"item"}, `{"item":null}`},
			{"top-level array", []string{"item"}, `[1,2,3]`},
			{"missing results key", []string{"results", "*", "item"}, `{"total":0}`},
			{"results is null", []string{"results", "*", "item"}, `{"results":null,"total":0}`},
			{"result entry missing item", []string{"results", "*", "item"}, `{"results":[{"snippet":"x"}]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := RewriteRawItemJSON(json.RawMessage(tc.raw), tc.path...)
				if string(got) != tc.raw {
					t.Errorf("changed unexpected shape:\n got: %s\nwant: %s", got, tc.raw)
				}
			})
		}
	})
}
