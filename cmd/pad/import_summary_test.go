package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrintImportSummary(t *testing.T) {
	t.Parallel()
	matched := importCounts{Collections: 9, Items: 412, Comments: 80, Attachments: 37}

	var out bytes.Buffer
	if !printImportSummary(&out, &importSummary{InBundle: matched, Imported: matched}) {
		t.Errorf("equal counts reported as a mismatch:\n%s", out.String())
	}
	if strings.Contains(out.String(), "does not match") || strings.Contains(out.String(), "counts differ") {
		t.Errorf("equal counts printed a shortfall:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "412") {
		t.Errorf("item count missing:\n%s", out.String())
	}

	short := matched
	short.Attachments = 36
	out.Reset()
	if printImportSummary(&out, &importSummary{InBundle: matched, Imported: short}) {
		t.Errorf("a missing attachment reported as matched:\n%s", out.String())
	}
	var flagged []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "does not match") {
			flagged = append(flagged, strings.Fields(line)[0])
		}
	}
	if len(flagged) != 1 || flagged[0] != "Attachments" {
		t.Errorf("flagged rows = %v, want only Attachments:\n%s", flagged, out.String())
	}
	if !strings.Contains(out.String(), "counts differ") {
		t.Errorf("a shortfall printed no explanation:\n%s", out.String())
	}
}

// The 201 keeps the workspace's own fields at the top level, so the summary
// wrapper must still decode them.
func TestImportedWorkspaceDecodesBothHalves(t *testing.T) {
	t.Parallel()
	body := `{"name":"Moved","slug":"moved","import_summary":{"in_bundle":{"items":2},"imported":{"items":2}}}`
	var ws importedWorkspace
	if err := json.Unmarshal([]byte(body), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Slug != "moved" || ws.Name != "Moved" {
		t.Errorf("workspace fields lost: %+v", ws.Workspace)
	}
	if ws.ImportSummary == nil || ws.ImportSummary.Imported.Items != 2 {
		t.Errorf("summary lost: %+v", ws.ImportSummary)
	}

	var old importedWorkspace
	if err := json.Unmarshal([]byte(`{"name":"Old","slug":"old"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.ImportSummary != nil {
		t.Errorf("a response with no summary decoded one: %+v", old.ImportSummary)
	}
}
