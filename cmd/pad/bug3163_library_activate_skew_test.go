package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3163 skew: a server older than this CLI ignores the typed `convention`
// member and creates the item without it, answering 201. The witness is the
// stored `convention` key, NOT item.Convention, which the server derives from
// the sibling keys this write also sends and which is therefore non-nil either
// way. The "siblings only" leg is the old server's real answer.
func TestVerifyConventionLandedCatchesAServerThatIgnoredTheMember(t *testing.T) {
	landed := &models.Item{Fields: `{"status":"active","trigger":"always","convention":{"trigger":"always"}}`}
	if err := verifyConventionLanded(landed); err != nil {
		t.Fatalf("a landed convention was refused: %v", err)
	}
	derived := &models.ItemConventionMetadata{Trigger: "always"}
	for name, item := range map[string]*models.Item{
		"siblings only, Convention derived": {Slug: "x", Fields: `{"status":"active","trigger":"always"}`, Convention: derived},
		"convention key null":               {Slug: "x", Fields: `{"convention":null}`},
		"empty fields":                      {Slug: "x"},
		"nil response":                      nil,
	} {
		err := verifyConventionLanded(item)
		if err == nil || !strings.Contains(err.Error(), "older than this CLI") {
			t.Errorf("%s: want the older-server error, got %v", name, err)
		}
	}
}

// The binding, driven through the real command against a fake server: the
// activate request carries the typed member and no reserved key in `fields`,
// and an old-server-shaped answer (no stored `convention` key) is reported as
// an error rather than as "Activated convention".
func TestLibraryActivateSendsTypedConventionAndVerifiesIt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		storesKey     bool
		wantActivated bool
	}{
		{name: "current server", storesKey: true, wantActivated: true},
		{name: "older server", storesKey: false, wantActivated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/convention-library", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"categories": []map[string]any{{
					"name": "quality",
					"conventions": []map[string]any{{
						"title": "Run tests", "content": "body", "category": "quality",
						"trigger": "on-commit", "surfaces": []string{"all"}, "enforcement": "must",
						"commands": []string{"make test"},
					}},
				}}})
			})
			mux.HandleFunc("/api/v1/workspaces/ws/collections", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{})
			})
			mux.HandleFunc("/api/v1/workspaces/ws/collections/conventions/items", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&posted)
				fields := map[string]any{"status": "active", "trigger": "on-commit"}
				if tc.storesKey {
					fields["convention"] = posted["convention"]
				}
				raw, _ := json.Marshal(fields)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "i1", "slug": "run-tests", "title": "Run tests", "fields": string(raw)})
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			setTempHomeMain(t)
			t.Setenv("PAD_URL", srv.URL)
			t.Setenv("PAD_TOKEN", "pad_testtoken")
			origWS, origFormat := workspaceFlag, formatFlag
			t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
			workspaceFlag, formatFlag = "ws", "table"

			cmd := libraryActivateCmd()
			cmd.SetArgs([]string{"Run tests"})
			var err error
			out := captureStdout(t, func() { err = cmd.Execute() })

			conv, ok := posted["convention"].(map[string]any)
			if !ok || conv["enforcement"] != "must" {
				t.Fatalf("request did not carry the typed convention member: %#v", posted)
			}
			var sentFields map[string]any
			if s, _ := posted["fields"].(string); json.Unmarshal([]byte(s), &sentFields) != nil {
				t.Fatalf("request fields not a JSON object string: %#v", posted["fields"])
			}
			if _, bad := sentFields["convention"]; bad {
				t.Errorf("request still sends the reserved key in fields: %v", sentFields)
			}
			if tc.wantActivated {
				if err != nil || !strings.Contains(out, "Activated convention") {
					t.Errorf("current server: want success, got err=%v out=%q", err, out)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "older than this CLI") {
					t.Errorf("older server: want the skew error, got err=%v", err)
				}
				if strings.Contains(out, "Activated convention") {
					t.Errorf("older server: reported success: %q", out)
				}
			}
		})
	}
}
