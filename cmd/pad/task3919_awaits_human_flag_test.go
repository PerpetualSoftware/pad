package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TASK-3919 (IDEA-3906): `pad collection update --awaits-human[=false]` sends
// the typed member (CONVE-19: wiring is a claim), and refuses to report
// success when the server did not apply it.
func TestTASK3919_ParseAwaitsHumanFlag(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "false": false, "yes": true, "no": false, " FALSE ": false} {
		got, err := parseAwaitsHumanFlag(in)
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	if _, err := parseAwaitsHumanFlag("maybe"); err == nil {
		t.Error("an unknown value was accepted")
	}
}

func TestTASK3919_CollectionUpdateSendsAwaitsHuman(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want any // the awaits_human member the PATCH carries; nil = absent
	}{
		{"bare flag", []string{"asks", "--awaits-human"}, true},
		{"explicit false", []string{"asks", "--awaits-human=false"}, false},
		{"not given", []string{"asks", "--icon", "x"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var patches []map[string]any
			setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				settings := "{}"
				if r.Method == http.MethodPatch {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					mu.Lock()
					patches = append(patches, body)
					mu.Unlock()
					if v, ok := body["awaits_human"].(bool); ok && v {
						settings = `{"awaits_human":true}`
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "slug": "asks", "name": "Asks", "settings": settings})
			}))

			cmd := collectionsUpdateCmd()
			cmd.SetArgs(tc.args)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			if err := cmd.Execute(); err != nil {
				t.Fatalf("update: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(patches) != 1 {
				t.Fatalf("PATCH count = %d, want 1", len(patches))
			}
			got, present := patches[0]["awaits_human"]
			if tc.want == nil {
				if present {
					t.Fatalf("sent awaits_human without the flag: %v", patches[0])
				}
				return
			}
			if got != tc.want {
				t.Fatalf("awaits_human = %v, want %v (%v)", got, tc.want, patches[0])
			}
		})
	}
}

func TestTASK3919_CollectionUpdateRefusesAnUnappliedAwaitsHuman(t *testing.T) {
	// An older server ignores the member and answers 200 with the settings unchanged.
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "slug": "asks", "name": "Asks", "settings": "{}"})
	}))
	cmd := collectionsUpdateCmd()
	cmd.SetArgs([]string{"asks", "--awaits-human"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "did not apply --awaits-human") {
		t.Fatalf("want the not-applied error, got %v", err)
	}
}
