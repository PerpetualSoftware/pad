package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestItemSearch_AllWorkspaces(t *testing.T) {
	previous := formatFlag
	t.Cleanup(func() { formatFlag = previous })

	var captured url.Values
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"item":{"id":"one","title":"Deploy guide","collection_name":"Docs","collection_icon":"📄"},"snippet":"deploy steps","matched_in":"content","rank":1.5,"workspace":{"slug":"eng-ops","name":"Eng Ops","owner_username":"alice"}}],"total":1,"limit":50,"offset":0,"facets":{"collections":{"docs":1},"statuses":{"open":1},"workspaces":{"eng-ops":1}}}`))
	}))

	// Clear workspaceFlag too to prove --all-workspaces does not require getWorkspace().
	workspaceFlag = ""
	formatFlag = "table"

	cmd := searchCmd()
	cmd.SetArgs([]string{"deploy", "--all-workspaces"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("search --all-workspaces: %v", err)
	}
	if _, hasWS := captured["workspace"]; hasWS {
		t.Errorf("workspace query param was sent (%q), want omitted", captured.Get("workspace"))
	}
	if captured.Get("q") != "deploy" {
		t.Errorf("q = %q, want %q", captured.Get("q"), "deploy")
	}
	if !strings.Contains(out, "📄 Deploy guide (Docs · eng-ops)") {
		t.Errorf("table output missing workspace slug:\n%s", out)
	}
}
