package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestNormalizeNativeItemCreateOutputMatchesStringEncoded(t *testing.T) {
	item := models.Item{
		ID:           "11111111-1111-1111-1111-111111111111",
		WorkspaceID:  "ws-1",
		CollectionID: "col-1",
		Ref:          "TASK-5",
		Title:        "Created item",
		Slug:         "created-item",
		Seq:          3,
		Fields:       `{"status":"open","priority":"high","implementation_notes":[{"summary":"note"}],"decision_log":[{"decision":"dec"}]}`,
		Tags:         `["alpha","beta"]`,
		ImplementationNotes: []models.ItemImplementationNote{
			{Summary: "note"},
		},
		DecisionLog: []models.ItemDecisionLogEntry{
			{Decision: "dec"},
		},
	}

	oldBytes, err := json.Marshal(&item)
	if err != nil {
		t.Fatalf("marshal old shape: %v", err)
	}
	newBytes, err := json.Marshal(cli.NativeItem(&item))
	if err != nil {
		t.Fatalf("marshal native shape: %v", err)
	}

	var oldParsed, newParsed any
	if err := json.Unmarshal(oldBytes, &oldParsed); err != nil {
		t.Fatalf("unmarshal old shape: %v", err)
	}
	if err := json.Unmarshal(newBytes, &newParsed); err != nil {
		t.Fatalf("unmarshal new shape: %v", err)
	}

	gotOld := normalizeStringEncodedJSONFields(oldParsed)
	gotNew := normalizeStringEncodedJSONFields(newParsed)
	if !reflect.DeepEqual(gotOld, gotNew) {
		t.Errorf("MCP normalizer produced different output for old vs native item create:\nold: %#v\nnew: %#v", gotOld, gotNew)
	}

	oldRes := packageJSONResult(string(oldBytes))
	newRes := packageJSONResult(string(newBytes))
	if !reflect.DeepEqual(oldRes.StructuredContent, newRes.StructuredContent) {
		t.Errorf("packageJSONResult StructuredContent differs for old vs native item create:\nold: %#v\nnew: %#v",
			oldRes.StructuredContent, newRes.StructuredContent)
	}
}
