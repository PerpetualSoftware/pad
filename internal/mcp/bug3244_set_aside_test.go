package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-3244 ruling 1 at the MCP doors: an agent told about set-aside edits must
// not be told that waiting, re-reading or opening the item stores them. Each
// test pairs the set-aside leg with the pending leg it must differ from.

func TestSetAsideHintMatchesAcrossPackages(t *testing.T) {
	if cli.ContentSetAsideHint != ContentSetAsideHint {
		t.Errorf("hints differ:\n cli %q\n mcp %q", cli.ContentSetAsideHint, ContentSetAsideHint)
	}
	if ContentSetAsideHint == ContentPendingFlushHint {
		t.Fatal("the set-aside hint must differ from the pending one")
	}
}

// Stdio: the CLI's marker line carries the hint the refusal's details select,
// and the classifier lifts it as-is.
func TestSetAsideHintSelectedOnStdio(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details string
		want    string
	}{
		{"set aside", `{"ref":"TASK-2","pending_rows":0,"set_aside_rows":1}`, ContentSetAsideHint},
		{"pending only", `{"ref":"TASK-2","pending_rows":3}`, ContentPendingFlushHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiErr := &cli.APIError{Code: cli.ContentPendingFlushCode, Message: "refused", Details: json.RawMessage(tc.details)}
			var stderr bytes.Buffer
			cli.WriteContentPendingFlushError(&stderr, apiErr)
			res := extractStructuredCLIError(stderr.String())
			if res == nil {
				t.Fatal("stdio classifier did not recognise the marker line")
			}
			env, ok := res.StructuredContent.(ErrorEnvelope)
			if !ok || env.Error.Hint != tc.want {
				t.Fatalf("hint = %q, want %q", env.Error.Hint, tc.want)
			}
		})
	}
}

// Remote: the same selection from the server's response body.
func TestSetAsideHintSelectedOnHTTP(t *testing.T) {
	if got := setAsideRowsIn(json.RawMessage(`{"set_aside_rows":2}`)); got != 2 {
		t.Fatalf("setAsideRowsIn = %d, want 2", got)
	}
	for _, raw := range []string{`{"pending_rows":3}`, ``, `not json`} {
		if got := setAsideRowsIn(json.RawMessage(raw)); got != 0 {
			t.Fatalf("setAsideRowsIn(%q) = %d, want 0", raw, got)
		}
	}
}

func TestReadItemSetAsideMarkerOffersNoTabRemedy(t *testing.T) {
	read := func(t *testing.T, state string) string {
		t.Helper()
		body := `{"ref":"TASK-5","title":"Fix OAuth","fields":"{}","content":"Plan.","content_state":"` + state + `"}`
		r := &resources{fetcher: &fakeFetcher{stdout: body}}
		req := mcp.ReadResourceRequest{}
		req.Params.URI = "pad://workspace/docapp/items/TASK-5"
		contents, err := r.readItem(context.Background(), req)
		if err != nil {
			t.Fatalf("readItem: %v", err)
		}
		return contents[0].(mcp.TextResourceContents).Text
	}
	setAside := read(t, "superseded_set_aside")
	if !strings.Contains(setAside, "Stale body") || !strings.Contains(setAside, "superseded_set_aside") {
		t.Fatalf("set-aside item carries no marker:\n%s", setAside)
	}
	if strings.Contains(setAside, "next flushes") || strings.Contains(setAside, "catches up") {
		t.Fatalf("set-aside marker promises a tab will catch the body up:\n%s", setAside)
	}
	if pending := read(t, "applied_pending_flush"); !strings.Contains(pending, "next flushes") {
		t.Fatalf("pending marker lost its wording:\n%s", pending)
	}
}
