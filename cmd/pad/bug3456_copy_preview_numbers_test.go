package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-3456: `pad item copy --dry-run` previews a carried number above 2^53
// with every digit, as the real copy writes it. Driven through the real
// client's preflight decode and the dry-run renderer, against a server that
// answers with the stored value.
func TestBUG3456_DryRunPreviewKeepsBigNumberDigits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fields":{"carried":[{"key":"n","label":"N","type":"number","value":9007199254740993,"from":"migrated"}],"dropped":[],"needs_value":[]}}`))
	}))
	defer ts.Close()
	t.Setenv("HOME", t.TempDir())

	c := cli.NewClientFromURL(ts.URL)
	pre, _, err := c.CopyItemPreflight("ws", "TASK-1", cli.ItemCopyRequest{TargetWorkspace: "b", TargetCollection: "tasks"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := renderItemCopyPreflight(&out, pre); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "= 9007199254740993") {
		t.Fatalf("dry run does not preview the stored digits:\n%s", out.String())
	}
	if strings.Contains(out.String(), "9007199254740992") {
		t.Fatalf("dry run previews the rounded value:\n%s", out.String())
	}
}
