package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3185: an export's suggested file name is "<slug>.pad.md", so an item
// slugged like a Windows device named a device. The name is prefixed "_",
// by the one device rule attachments use (BUG-2822).
func TestArtifactExportFilename_BUG3185_DeviceStemIsPrefixed(t *testing.T) {
	cases := []struct{ slug, want string }{
		{"nul", "_nul.pad.md"},
		{"con", "_con.pad.md"},
		{"com1", "_com1.pad.md"},
		{"lpt9", "_lpt9.pad.md"},
		{"aux", "_aux.pad.md"},
		// Near misses: a longer stem is not a device.
		{"nulls", "nulls.pad.md"},
		{"com10", "com10.pad.md"},
		{"con-x", "con-x.pad.md"},
		{"ship-it", "ship-it.pad.md"},
	}
	for _, c := range cases {
		if got := artifactExportFilename(&models.Item{Slug: c.slug}); got != c.want {
			t.Errorf("slug %q: got %q, want %q", c.slug, got, c.want)
		}
	}
}

// The binding leg: the export ENDPOINT carries the prefixed name in its
// Content-Disposition, which is what both clients save under.
func TestExportArtifact_BUG3185_DeviceSlugHeaderIsPrefixed(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)
	pb := createItem(t, srv, ws, "playbooks", map[string]interface{}{
		"title":   "NUL",
		"content": "## Steps\n\n1. Nothing\n",
		"fields":  `{"status":"active","trigger":"manual","scope":"all"}`,
	})
	if pb.Slug != "nul" {
		t.Fatalf("precondition: the title must slug to exactly %q to exercise the device rule, got %q", "nul", pb.Slug)
	}
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+pb.Slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	cd := rr.Header().Get("Content-Disposition")
	if !strings.Contains(cd, `filename="_nul.pad.md"`) {
		t.Errorf("Content-Disposition %q does not name _nul.pad.md", cd)
	}
}
