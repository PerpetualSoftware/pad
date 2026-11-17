package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestSearchEntryMarker(t *testing.T) {
	previous := formatFlag
	t.Cleanup(func() { formatFlag = previous })
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[` +
			`{"item":{"id":"one","title":"Noted","collection_name":"Tasks"},"snippet":"nebulaquartz note context","matched_in":"note","entry_id":"note-one","rank":0},` +
			`{"item":{"id":"two","title":"Decided","collection_name":"Tasks"},"snippet":"nebulaquartz decision context","matched_in":"decision","entry_id":"decision-two","rank":0}` +
			`],"total":2,"limit":50,"offset":0}`))
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
				for _, want := range []string{`"matched_in": "note"`, `"entry_id": "note-one"`, `"matched_in": "decision"`, `"entry_id": "decision-two"`} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %s: %s", want, out)
					}
				}
				return
			}
			for _, want := range []string{"nebulaquartz note context (in note)", "nebulaquartz decision context (in decision)"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
		})
	}
}
