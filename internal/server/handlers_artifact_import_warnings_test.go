package server

// BUG-2850: the artifact-import door must report the create's
// undeclared-field warnings.
//
// This PR's claim is that an undeclared key is STORED and NAMED, so a typo
// leaves a trace. createItemChecked computes exactly that; the artifact-import
// handler builds its OWN response shape and was returning only the
// preprocessing warnings.
//
// READ THE SETUP BEFORE CHANGING IT — it is what makes the case reachable,
// and I got this wrong once. Round 10 dismissed the finding as unreachable
// because artifact.Decode populates Fields only from FieldKeysForKind. That
// check was true and irrelevant: UndeclaredFieldKeys compares the field map
// against the DESTINATION COLLECTION'S SCHEMA, not against the artifact
// format's key list. So the reachable shape is not a weird artifact — it is
// an ordinary artifact meeting a NARROWED destination schema, which is what
// this test builds.
//
// The trait is unique per kind (a second collection declaring
// artifact_kind=convention is refused 409), so the destination is always the
// seeded conventions collection — narrowing its schema, not adding a rival
// collection, is the route.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
)

func TestImportArtifactReportsUndeclaredFieldsAgainstANarrowedSchema(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// Narrow the destination so trigger/scope/priority — all canonical
	// convention artifact fields — are no longer declared there.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/collections/conventions", map[string]any{
		"schema": `{"fields":[{"key":"status","type":"select","options":["draft","active"]}]}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("narrow conventions schema: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Convention meeting a narrowed destination",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"},
		Body:          "Perfectly legal artifact; the destination just declares less.\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr2 := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr2.Code, rr2.Body.String())
	}

	var resp struct {
		Ref      string   `json:"ref"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rr2.Body.String())
	}

	joined := strings.Join(resp.Warnings, "\n")
	for _, key := range []string{"trigger", "scope", "priority"} {
		if !strings.Contains(joined, key) {
			t.Errorf("undeclared key %q was stored but not named in the import warnings; got %v", key, resp.Warnings)
		}
	}
}

// ...and an import into an INTACT destination reports no undeclared-field
// warning, so the channel does not cry wolf on the ordinary case. This is
// also the control that shows the test above is testing the narrowing and not
// merely the presence of the keys.
func TestImportArtifactAgainstTheDefaultSchemaNamesNothingUndeclared(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Convention meeting the seeded destination",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"},
		Body:          "Nothing foreign here.\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rr.Body.String())
	}
	for _, w := range resp.Warnings {
		if strings.Contains(w, "not declared") {
			t.Errorf("an artifact matching its destination must not produce an undeclared-field warning; got %q", w)
		}
	}
}
