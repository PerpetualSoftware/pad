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

// The import response must forward BOTH halves of ItemWriteWarnings (codex
// round 8).
//
// `createItemChecked` records dropped relation defaults alongside undeclared
// keys; this handler enumerated only the undeclared ones, so an import that
// discarded a value said nothing about it. That is the gap a per-member
// forwarding loop opens every time the struct it reads from grows — and a
// discarded value is worse news than a stored-but-unrecognized one, because it
// is gone.
//
// Same route as the test above: the destination's schema is what makes the
// case reachable, not a weird artifact. A relation field whose DEFAULT is a
// number is never type-checked by ValidateFields, so it reaches the write and
// is dropped there.
func TestImportArtifactReportsDroppedRelationDefault(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/collections/conventions", map[string]any{
		"schema": `{"fields":[{"key":"status","type":"select","options":["draft","active"]},{"key":"owner_ref","type":"relation","collection":"nobody","default":42}]}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("add a bad relation default: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Convention meeting a broken relation default",
		Fields:        map[string]any{"status": "active"},
		Body:          "Body.\n",
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
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rr2.Body.String())
	}
	if !strings.Contains(strings.Join(resp.Warnings, "\n"), "owner_ref") {
		t.Fatalf("the import discarded owner_ref without saying so: %v", resp.Warnings)
	}
}

// IMPORT CARRIES a junk relation value; it never refuses (Dave's ruling,
// day 57: "explicitly allow import of the junk to avoid breaking import").
//
// Import is a CARRY door, not a write door. An artifact was written elsewhere,
// possibly before referent validation existed, and the person importing it
// neither chose its field values nor can fix them from the import call.
// Refusing would break exactly the artifacts most likely to hold junk.
//
// Not the migrate doors' carry either: those DROP what they cannot resolve.
// The artifact is the record, so the value is kept — and reported, so the
// import is never silently lossy.
func TestImportArtifactCarriesUnresolvableRelationValue(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/collections/conventions", map[string]any{
		// `role` rather than an invented key: the artifact FORMAT decides which
		// keys reach the field map at all (artifact.Decode populates Fields
		// from FieldKeysForKind), so a relation field the format does not know
		// is filtered out before this door and the case is unreachable. It has
		// to be a canonical convention key that the DESTINATION declares as a
		// relation. My first fixture used `owner_ref` and asserted a carry
		// that decode had already dropped.
		"schema": `{"fields":[{"key":"status","type":"select","options":["draft","active"]},{"key":"role","type":"relation","collection":"nobody"}]}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("add a relation field: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Convention carrying a junk relation",
		Fields:        map[string]any{"status": "active", "role": "just some text"},
		Body:          "Body.\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr2 := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("import REFUSED a junk relation value (%d); import carries, never refuses: %s",
			rr2.Code, rr2.Body.String())
	}
	var resp struct {
		Ref      string   `json:"ref"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, rr2.Body.String())
	}

	// KEPT, not dropped: the artifact is the record. Read back through the
	// API rather than the store, so this asserts what a consumer sees.
	show := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+resp.Ref, nil)
	if show.Code != http.StatusOK {
		t.Fatalf("GET the imported item: %d %s", show.Code, show.Body.String())
	}
	if !strings.Contains(show.Body.String(), "just some text") {
		t.Fatalf("import discarded the junk value instead of carrying it: %s", show.Body.String())
	}
	// ...and reported, so the import is not silently lossy.
	if !strings.Contains(strings.Join(resp.Warnings, "\n"), "role") {
		t.Fatalf("import carried an unresolvable relation without saying so: %v", resp.Warnings)
	}
}

// An import carrying ONE unresolvable caller value must still clean up the
// destination schema's own broken defaults (codex round 15).
//
// The write path's step 2 returned early when the caller's values had issues —
// correct while every caller of that path REFUSED, since steps 3 and 4 only
// prepare a field map nobody stores. The carry posture made a caller that does
// not refuse, and the early return stayed: an artifact holding one junk
// relation skipped the late-default pass AND the default-visibility drop
// entirely, storing whatever the destination schema injected and reporting
// none of it.
//
// This fixture is the two existing artifact-relation tests composed, and the
// composition is the whole point — each half passes on its own against the
// unfixed build, and only together do they reach the skipped steps:
//
//   - `role`, an unresolvable CALLER value, which is what fires the early
//     return (TestImportArtifactCarriesUnresolvableRelationValue);
//   - `owner_ref`, a broken destination DEFAULT that step 3 is what drops
//     (TestImportArtifactReportsDroppedRelationDefault).
func TestImportArtifactCleansBrokenDefaultsDespiteACarriedJunkValue(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/collections/conventions", map[string]any{
		"schema": `{"fields":[{"key":"status","type":"select","options":["draft","active"]},{"key":"role","type":"relation","collection":"nobody"},{"key":"owner_ref","type":"relation","collection":"nobody","default":42}]}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("narrow conventions schema: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Convention with junk AND a broken default",
		Fields:        map[string]any{"status": "active", "role": "just some text"},
		Body:          "Body.\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr2 := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("import REFUSED (%d); import carries, never refuses: %s", rr2.Code, rr2.Body.String())
	}
	var resp struct {
		Ref      string   `json:"ref"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, rr2.Body.String())
	}
	warnings := strings.Join(resp.Warnings, "\n")

	// CONTROL: the carried junk value still reaches the report. If this leg
	// fails the fixture never fired the early return, and the assertion below
	// would be measuring a path that was never skipped.
	if !strings.Contains(warnings, "role") {
		t.Fatalf("the carried junk value was not reported, so the early return this test "+
			"exists to reach was never taken: %v", resp.Warnings)
	}

	show := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+resp.Ref, nil)
	if show.Code != http.StatusOK {
		t.Fatalf("GET the imported item: %d %s", show.Code, show.Body.String())
	}
	if strings.Contains(show.Body.String(), `"owner_ref":42`) {
		t.Fatalf("a carried junk value made the import skip the late-default pass, and the "+
			"schema's broken default was stored raw: %s", show.Body.String())
	}
	if !strings.Contains(warnings, "owner_ref") {
		t.Fatalf("the broken destination default was neither cleaned nor reported: %v", resp.Warnings)
	}
}
