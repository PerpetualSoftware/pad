package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCollectionTraitsSurviveOrdinarySchemaEdit is the property that decided
// traits get their own column instead of a key inside the schema JSON.
//
// Every client rebuilds the schema blob as {"fields":[...]} — the web
// collection editor, the webmcp dispatcher, the quick-actions menu — and the
// store overwrites the column wholesale. A traits key stored in there was
// measurably destroyed by one ordinary edit during TASK-2657, which would
// have made kernel behavior something an unrelated UI save could silently
// disarm. This test locks the fix: an update that says nothing about traits
// leaves them exactly as they were.
func TestCollectionTraitsSurviveOrdinarySchemaEdit(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	before := collectionTraits(t, srv, slug, "conventions")
	if len(before.BootstrapInclude) != 2 {
		t.Fatalf("control leg failed: seeded conventions should declare 2 bootstrap includes, got %d", len(before.BootstrapInclude))
	}

	// An ordinary schema edit, in the exact shape EditCollectionModal sends.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/conventions", map[string]interface{}{
		"schema": `{"fields":[{"key":"status","type":"select","options":["active","draft","disabled"]}]}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("schema edit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	after := collectionTraits(t, srv, slug, "conventions")
	if len(after.BootstrapInclude) != len(before.BootstrapInclude) {
		t.Errorf("bootstrap_include declarations lost: %d before, %d after", len(before.BootstrapInclude), len(after.BootstrapInclude))
	}
	if after.ArtifactKind == nil || before.ArtifactKind == nil || after.ArtifactKind.Kind != before.ArtifactKind.Kind {
		t.Errorf("artifact_kind lost: %+v before, %+v after", before.ArtifactKind, after.ArtifactKind)
	}

	// And the behavior those declarations drive still works end to end.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	var b AgentBootstrap
	parseJSON(t, rr, &b)
	if b.ConventionIndex == nil {
		t.Error("convention_index went nil after an unrelated schema edit")
	}
}

// TestCollectionTraitsExplicitClear proves the preservation above is not a
// one-way door: a caller that MEANS to clear declarations still can.
func TestCollectionTraitsExplicitClear(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/conventions", map[string]interface{}{
		"traits": map[string]interface{}{},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear traits: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	after := collectionTraits(t, srv, slug, "conventions")
	if !after.IsZero() {
		t.Errorf("explicit clear left declarations behind: %+v", after)
	}
}

// TestCollectionTraitsRejectMalformed locks the fail-loud gate (SPEC-0 L6).
// A declaration that cannot be parsed or validated must be REFUSED, because a
// stored-but-unparseable blob degrades to "declares nothing" — silently the
// wrong behavior rather than a loud error, which is the exact failure mode
// traits exist to remove.
func TestCollectionTraitsRejectMalformed(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	cases := []struct {
		name   string
		traits interface{}
	}{
		{
			name:   "unknown mode",
			traits: map[string]interface{}{"bootstrap_include": []map[string]interface{}{{"mode": "summaries", "key": "x"}}},
		},
		{
			name:   "missing key",
			traits: map[string]interface{}{"bootstrap_include": []map[string]interface{}{{"mode": "bodies"}}},
		},
		{
			name:   "invocation_field naming another field",
			traits: map[string]interface{}{"invocation_field": "route_slug"},
		},
		{
			name:   "misspelled trait name",
			traits: map[string]interface{}{"bootstrap_includes": []map[string]interface{}{{"mode": "bodies", "key": "x"}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/conventions", map[string]interface{}{
				"traits": tc.traits,
			})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %s, got %d: %s", tc.name, rr.Code, rr.Body.String())
			}
			// The refusal must not have partially applied: the seeded
			// declarations are still intact.
			if got := collectionTraits(t, srv, slug, "conventions"); len(got.BootstrapInclude) != 2 {
				t.Errorf("a refused update damaged the stored declarations: %+v", got)
			}
		})
	}
}

// TestSeededCollectionsDeclareTraits pins that templates — not just the
// migration — declare the traits, so a NEWLY created workspace never depends
// on the backfill having run.
func TestSeededCollectionsDeclareTraits(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	conv := collectionTraits(t, srv, slug, "conventions")
	if conv.BootstrapIncludeForKey("conventions") == nil {
		t.Error("conventions collection declares no bodies include")
	}
	if conv.BootstrapIncludeForKey("convention_index") == nil {
		t.Error("conventions collection declares no metadata index include")
	}
	if conv.ArtifactKind == nil || conv.ArtifactKind.Kind != "convention" {
		t.Errorf("conventions artifact_kind = %+v, want convention", conv.ArtifactKind)
	}
	// The bodies include must carry status=active: without it, DRAFT
	// conventions reach agents as though they were policy.
	if inc := conv.BootstrapIncludeForKey("conventions"); inc != nil {
		if inc.Filter["status"] != "active" || inc.Filter["trigger"] != "always" {
			t.Errorf("conventions bodies filter = %+v, want status=active trigger=always", inc.Filter)
		}
	}

	pb := collectionTraits(t, srv, slug, "playbooks")
	if pb.InvocationField != models.InvocationSlugField {
		t.Errorf("playbooks invocation_field = %q, want %q", pb.InvocationField, models.InvocationSlugField)
	}
	if pb.ArtifactKind == nil || pb.ArtifactKind.Kind != "playbook" {
		t.Errorf("playbooks artifact_kind = %+v, want playbook", pb.ArtifactKind)
	}
	// Playbooks deliberately carries NO status filter — an agent needs to see
	// that a draft playbook exists (the run gate refuses it separately).
	if inc := pb.BootstrapIncludeForKey("playbooks"); inc == nil {
		t.Error("playbooks collection declares no include")
	} else if len(inc.Filter) != 0 {
		t.Errorf("playbooks include filter = %+v, want empty", inc.Filter)
	}
}

// collectionTraits fetches a collection over the API and parses its traits.
func collectionTraits(t *testing.T, srv *Server, wsSlug, collSlug string) models.CollectionTraits {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/collections/"+collSlug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get collection %s: expected 200, got %d: %s", collSlug, rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	traits, err := models.ParseCollectionTraits(coll.Traits)
	if err != nil {
		t.Fatalf("parse traits for %s: %v (raw: %q)", collSlug, err, coll.Traits)
	}
	return traits
}

// TestArtifactExportRefusesUnknownDeclaredKind covers the gap between "any
// non-empty kind is a legal declaration" (SPEC-5) and "this build knows how to
// serialize it". An unknown kind used to reach artifact.Encode, which returns
// ErrUnknownKind and surfaced as a 500. A collection whose declared kind this
// build cannot encode is simply not exportable — a 400, exactly like a
// collection declaring no kind at all. Codex round 1.
func TestArtifactExportRefusesUnknownDeclaredKind(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/conventions", map[string]interface{}{
		"traits": map[string]interface{}{"artifact_kind": map[string]string{"kind": "widget"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("declare unknown kind: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/conventions/items", map[string]interface{}{
		"title":  "Some rule",
		"fields": map[string]string{"status": "active"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created map[string]interface{}
	parseJSON(t, rr, &created)
	itemSlug, _ := created["slug"].(string)
	if itemSlug == "" {
		t.Fatal("created item has no slug")
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+itemSlug+"/export", nil)
	if rr.Code == http.StatusInternalServerError {
		t.Errorf("unknown declared kind produced a 500; want a 4xx refusal: %s", rr.Body.String())
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("export with unknown declared kind = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

// TestResolvePlaybookIgnoresInvisibleCollections locks the shadowing fix from
// Codex round 2.
//
// Before traits this was structurally impossible: resolution named one
// collection. Now any number may declare invocation_field, so resolving across
// all of them and rejecting afterwards on visibility lets a HIDDEN collection
// shadow a visible one — the resolver returns the hidden item, the caller's
// visibility check refuses it, and the visible playbook carrying the same
// invocation slug becomes unreachable.
//
// The setup deliberately makes the HIDDEN collection sort FIRST (it is the
// seeded `playbooks`, created before the second one), so unfixed code returns
// the hidden item and the test fails. If the visible collection sorted first
// the assertion would pass either way and prove nothing.
func TestResolvePlaybookIgnoresInvisibleCollections(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)

	// A second collection that also routes by invocation slug.
	//
	// Created WITHOUT traits through the API and then given them via the
	// store, because the API deliberately REFUSES a second invocation-routing
	// declaration (409, SPEC-0 L6). A duplicate can still arrive by routes the
	// gate does not sit on — a workspace import, or a rename that frees the
	// canonical slug for a new collection — and this test is about what
	// resolution does when one exists, not about how it got there.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]interface{}{
		"name":   "Procedures",
		"schema": `{"fields":[{"key":"status","type":"select","options":["active","draft"]},{"key":"invocation_slug","type":"text","unique_scope":"workspace_collection"}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create second routing collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var second models.Collection
	parseJSON(t, rr, &second)
	secondTraits := `{"bootstrap_include":[{"mode":"metadata","key":"playbooks"}],"invocation_field":"invocation_slug"}`
	if _, err := srv.store.UpdateCollection(second.ID, models.CollectionUpdate{Traits: &secondTraits}); err != nil {
		t.Fatalf("attach traits to second collection: %v", err)
	}

	// Same invocation slug in both collections. The partial unique index is
	// per (collection_id, invocation_slug), so this is legal.
	for _, collSlug := range []string{"playbooks", second.Slug} {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+collSlug+"/items", map[string]interface{}{
			"title":  "Ship from " + collSlug,
			"fields": map[string]string{"status": "active", "invocation_slug": "ship"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create playbook in %s: expected 201, got %d: %s", collSlug, rr.Code, rr.Body.String())
		}
	}

	wsID := workspaceIDForSlug(t, srv, wsSlug)

	// Control leg: unrestricted resolution finds something at all, and finds
	// the FIRST declaring collection — the seeded playbooks one. If this ever
	// returns the second collection's item the test below is vacuous.
	unrestricted, err := srv.resolvePlaybook(wsID, "ship", nil)
	if err != nil {
		t.Fatalf("control leg failed: unrestricted resolve errored: %v", err)
	}
	if unrestricted.CollectionSlug != "playbooks" {
		t.Fatalf("control leg failed: unrestricted resolve returned %q, expected the seeded playbooks collection to sort first; the visibility assertion below would not discriminate", unrestricted.CollectionSlug)
	}

	// Now resolve as a caller who can see ONLY the second collection. The
	// seeded playbooks collection must be invisible to resolution, not merely
	// unreadable afterwards.
	got, err := srv.resolvePlaybook(wsID, "ship", []string{second.ID})
	if err != nil {
		t.Fatalf("restricted resolve errored: %v — a hidden collection shadowed the visible one", err)
	}
	if got.CollectionSlug != second.Slug {
		t.Errorf("restricted resolve returned the playbook in %q; want the one in the only visible collection %q", got.CollectionSlug, second.Slug)
	}

	// A caller who can see NEITHER declaring collection gets a not-found,
	// never someone else's item.
	if _, err := srv.resolvePlaybook(wsID, "ship", []string{}); err == nil {
		t.Error("resolve with no visible routing collections returned an item; want not-found")
	}
}

// TestCollectionIDForKindIgnoresInvisibleCollections is the artifact-import
// half of the round-2 shadowing fix. Same failure shape as playbook
// resolution: with two collections declaring one artifact kind, selecting the
// first and checking visibility afterwards means a hidden collection makes the
// import fail even though a destination the caller can write to exists.
//
// As above, the HIDDEN collection is the seeded one so it sorts first —
// otherwise the assertion passes with or without the fix.
func TestCollectionIDForKindIgnoresInvisibleCollections(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)

	// Same shape as the playbook test above: the API refuses a duplicate
	// artifact_kind declaration, so the ambiguous state is built through the
	// store, which is how it genuinely arises (import, or a rename freeing the
	// canonical slug).
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]interface{}{
		"name":   "House Rules",
		"schema": `{"fields":[{"key":"status","type":"select","options":["active","draft"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create second declaring collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var second models.Collection
	parseJSON(t, rr, &second)
	secondTraits := `{"artifact_kind":{"kind":"convention"}}`
	if _, err := srv.store.UpdateCollection(second.ID, models.CollectionUpdate{Traits: &secondTraits}); err != nil {
		t.Fatalf("attach traits to second collection: %v", err)
	}

	wsID := workspaceIDForSlug(t, srv, wsSlug)

	seeded := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/collections/conventions", nil)
	var seededColl models.Collection
	parseJSON(t, seeded, &seededColl)

	// Control leg: unrestricted selection picks the seeded collection, so the
	// restricted assertion below genuinely discriminates.
	unrestricted, err := srv.collectionIDForKind(wsID, "convention", nil)
	if err != nil {
		t.Fatalf("control leg failed: %v", err)
	}
	if unrestricted != seededColl.ID {
		t.Fatalf("control leg failed: unrestricted selection returned %q, expected the seeded conventions collection %q to sort first", unrestricted, seededColl.ID)
	}

	got, err := srv.collectionIDForKind(wsID, "convention", []string{second.ID})
	if err != nil {
		t.Fatalf("restricted selection errored: %v", err)
	}
	if got != second.ID {
		t.Errorf("restricted selection returned %q; want the only visible declaring collection %q — a hidden collection shadowed it", got, second.ID)
	}

	if none, err := srv.collectionIDForKind(wsID, "convention", []string{}); err != nil || none != "" {
		t.Errorf("selection with no visible declaring collection = (%q, %v); want (\"\", nil)", none, err)
	}
}

// TestGenericBootstrapIncludeIsBudgeted locks the SPEC-0 L4 boot budget on the
// generic include path (Codex round 5). Without a cap, one declaration —
// bodies mode, no filter, on a large collection — makes every agent's boot
// arbitrarily large, and nothing in the trait grammar prevents declaring it.
//
// Also asserts the overflow signal: an agent must be able to tell a truncated
// payload from a complete one, or it reads a prefix as the whole set.
func TestGenericBootstrapIncludeIsBudgeted(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]interface{}{
		"name":   "Notes",
		"schema": `{"fields":[{"key":"status","type":"select","options":["active","draft"]}]}`,
		"traits": map[string]interface{}{
			"bootstrap_include": []map[string]interface{}{{"mode": "bodies", "key": "notes"}},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create declaring collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var notes models.Collection
	parseJSON(t, rr, &notes)

	const created = bootstrapGenericIncludeCap + 5
	for i := 0; i < created; i++ {
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+notes.Slug+"/items", map[string]interface{}{
			"title":   fmt.Sprintf("Note %d", i),
			"content": "body",
			"fields":  map[string]string{"status": "active"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create note %d: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: expected 200, got %d", rr.Code)
	}
	var b AgentBootstrap
	parseJSON(t, rr, &b)

	var group *BootstrapIncludeGroup
	for i := range b.BootstrapIncludes {
		if b.BootstrapIncludes[i].Key == "notes" {
			group = &b.BootstrapIncludes[i]
		}
	}
	if group == nil {
		t.Fatal("control leg failed: the declared payload did not appear in bootstrap at all")
	}
	if len(group.Items) > bootstrapGenericIncludeCap {
		t.Errorf("generic include shipped %d items; the boot budget caps it at %d", len(group.Items), bootstrapGenericIncludeCap)
	}
	if group.OverflowCount == 0 {
		t.Errorf("payload was truncated (%d of %d items) but overflow_count is 0 — an agent would read a prefix as the complete set", len(group.Items), created)
	}
}

// TestTraitConflictsRefused locks SPEC-0 L6 at the one surface that can MINT
// an ambiguous declaration. Two collections declaring the same artifact_kind,
// or two declaring invocation_field, leave the resolvers picking by collection
// order — sort_order then created_at — which makes where an imported artifact
// lands, or which collection answers an invocation slug, a coin flip. Refused
// with a conflict rather than resolved silently. Codex round 7.
func TestTraitConflictsRefused(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)

	cases := []struct {
		name   string
		traits map[string]interface{}
	}{
		{
			name:   "duplicate artifact_kind",
			traits: map[string]interface{}{"artifact_kind": map[string]string{"kind": "convention"}},
		},
		{
			name:   "second invocation-routing collection",
			traits: map[string]interface{}{"invocation_field": "invocation_slug"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]interface{}{
				"name":   "Rival " + tc.name,
				"schema": `{"fields":[{"key":"status","type":"select","options":["active"]}]}`,
				"traits": tc.traits,
			})
			if rr.Code != http.StatusConflict {
				t.Errorf("expected 409 for %s, got %d: %s", tc.name, rr.Code, rr.Body.String())
			}
		})
	}

	// Control leg: a declaration that conflicts with NOTHING is accepted, so
	// the refusals above are about the conflict and not about traits on
	// create generally.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]interface{}{
		"name":   "Notes",
		"schema": `{"fields":[{"key":"status","type":"select","options":["active"]}]}`,
		"traits": map[string]interface{}{
			"bootstrap_include": []map[string]interface{}{{"mode": "bodies", "key": "notes"}},
			"artifact_kind":     map[string]string{"kind": "note"},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("control leg failed: a non-conflicting declaration was refused: %d %s", rr.Code, rr.Body.String())
	}

	// A collection must not conflict with ITSELF on update.
	var notes models.Collection
	parseJSON(t, rr, &notes)
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/collections/"+notes.Slug, map[string]interface{}{
		"traits": map[string]interface{}{
			"bootstrap_include": []map[string]interface{}{{"mode": "metadata", "key": "notes"}},
			"artifact_kind":     map[string]string{"kind": "note"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Errorf("a collection conflicted with its own declarations on update: %d %s", rr.Code, rr.Body.String())
	}
}

// TestFirstPartyKeyModeIsPinned covers the other round-7 contract gap: the
// three first-party payloads have fixed projection shapes, so a declaration
// naming one with the other mode would be quietly ignored — the payload comes
// out in the projection's shape regardless of what the declaration said.
func TestFirstPartyKeyModeIsPinned(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/collections/conventions", map[string]interface{}{
		"traits": map[string]interface{}{
			"bootstrap_include": []map[string]interface{}{{"mode": "metadata", "key": "conventions"}},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("metadata mode on the bodies-shaped `conventions` payload = %d, want 400: %s", rr.Code, rr.Body.String())
	}

	// Control leg: the same key with its own mode is accepted.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/collections/conventions", map[string]interface{}{
		"traits": map[string]interface{}{
			"bootstrap_include": []map[string]interface{}{{"mode": "bodies", "key": "conventions"}},
		},
	})
	if rr.Code != http.StatusOK {
		t.Errorf("control leg failed: bodies mode on `conventions` was refused: %d %s", rr.Code, rr.Body.String())
	}
}
