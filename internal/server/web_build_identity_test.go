package server

import (
	"net/http"
	"testing"
	"testing/fstest"
)

func webFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for p, c := range files {
		m[p] = &fstest.MapFile{Data: []byte(c)}
	}
	return m
}

func TestIdentifyWebBuild_DigestTracksContentAndPaths(t *testing.T) {
	base := map[string]string{
		"index.html":              "<html></html>",
		"_app/immutable/start.js": "console.log(1)",
	}
	a := identifyWebBuild(webFS(base))
	if a == nil || a.SHA256 == "" || a.Files != 2 {
		t.Fatalf("identity: %+v", a)
	}
	if b := identifyWebBuild(webFS(base)); b.SHA256 != a.SHA256 {
		t.Errorf("digest is not deterministic: %s vs %s", a.SHA256, b.SHA256)
	}

	changed := map[string]string{"index.html": "<html></html>", "_app/immutable/start.js": "console.log(2)"}
	if c := identifyWebBuild(webFS(changed)); c.SHA256 == a.SHA256 {
		t.Error("a content change did not change the digest")
	}
	renamed := map[string]string{"index.html": "<html></html>", "_app/immutable/start2.js": "console.log(1)"}
	if r := identifyWebBuild(webFS(renamed)); r.SHA256 == a.SHA256 {
		t.Error("a path change did not change the digest")
	}
	if a.SourceCommit != nil || a.SourceDirty != nil {
		t.Errorf("no stamp, but source fields set: %+v", a)
	}
}

func TestIdentifyWebBuild_ReadsStamp(t *testing.T) {
	id := identifyWebBuild(webFS(map[string]string{
		"index.html":       "x",
		webBuildSourceFile: `{"commit":"abc123","dirty":2}`,
	}))
	if id.SourceCommit == nil || *id.SourceCommit != "abc123" {
		t.Errorf("source_commit: %v", id.SourceCommit)
	}
	if id.SourceDirty == nil || *id.SourceDirty != 2 {
		t.Errorf("source_dirty: %v", id.SourceDirty)
	}

	// A stamp written where git was unavailable carries nulls: the fields
	// stay absent rather than claiming a clean tree at no commit.
	unknown := identifyWebBuild(webFS(map[string]string{
		"index.html":       "x",
		webBuildSourceFile: `{"commit":null,"dirty":null}`,
	}))
	if unknown.SourceCommit != nil || unknown.SourceDirty != nil {
		t.Errorf("null stamp should leave source fields absent: %+v", unknown)
	}
}

func TestHealth_ReportsServedWebBundle(t *testing.T) {
	srv := testServer(t)
	srv.SetWebUI(webFS(map[string]string{
		"index.html":       "<html></html>",
		webBuildSourceFile: `{"commit":"abc123","dirty":0}`,
	}))

	rr := doRequest(srv, http.MethodGet, "/api/v1/health", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("health: %d", rr.Code)
	}
	var resp struct {
		Web *struct {
			SHA256       string  `json:"sha256"`
			Files        int     `json:"files"`
			SourceCommit *string `json:"source_commit"`
			SourceDirty  *int    `json:"source_dirty"`
		} `json:"web"`
	}
	parseJSON(t, rr, &resp)
	if resp.Web == nil {
		t.Fatalf("health has no web member: %s", rr.Body.String())
	}
	if resp.Web.SHA256 != srv.webIdentity.SHA256 || resp.Web.Files != 2 {
		t.Errorf("web: %+v", resp.Web)
	}
	if resp.Web.SourceCommit == nil || *resp.Web.SourceCommit != "abc123" || resp.Web.SourceDirty == nil || *resp.Web.SourceDirty != 0 {
		t.Errorf("web source: %+v", resp.Web)
	}
}

func TestHealth_NoWebUI_NoWebMember(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, http.MethodGet, "/api/v1/health", nil)
	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	if _, ok := resp["web"]; ok {
		t.Errorf("health reported a web bundle with no web UI set: %v", resp["web"])
	}
}
