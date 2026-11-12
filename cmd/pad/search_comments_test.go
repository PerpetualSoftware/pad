package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestSearchCommentMarker(t *testing.T) {
	previous := formatFlag
	t.Cleanup(func() { formatFlag = previous })
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"item":{"id":"one","title":"Plain title","collection_name":"Tasks"},"snippet":"nebulaquartz comment context","matched_in":"comment","comment_id":"comment-one","rank":0}],"total":1,"limit":50,"offset":0}`))
	}))
	for _, format := range []string{"table", "markdown", "json"} {
		t.Run(format, func(t *testing.T) {
			formatFlag = format
			cmd := searchCmd()
			cmd.SetArgs([]string{"nebulaquartz"})
			var err error
			out := captureStdout(t, func() { err = cmd.Execute() })
			if err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				if !strings.Contains(out, `"matched_in": "comment"`) || !strings.Contains(out, `"comment_id": "comment-one"`) {
					t.Errorf("missing source: %s", out)
				}
			} else if !strings.Contains(out, "in comment") || !strings.Contains(out, "nebulaquartz comment context") {
				t.Errorf("missing comment marker: %s", out)
			}
		})
	}
}
