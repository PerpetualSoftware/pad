package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3033 — the doors that do NOT serialise models.Item and therefore inherit
// nothing from BUG-3000.
//
// Every test here runs BOTH directions on ONE item, and asserts ABSENCE FIRST.
// That order is the instrument, not decoration: a marker stamped unconditionally
// would satisfy every positive assertion in this file, so a presence leg proves
// something only after the same door has been shown to stay silent about a
// current row.
//
// Assertions are on the RAW wire form (a decoded map, or the exported bytes)
// rather than on a Go struct. A struct round-trip passes even when the field is
// never serialised, which is precisely the failure mode — a value computed
// correctly in the store and dropped by a projection on the way out.
//
// makeStale puts the op-log ahead of the item's flush watermark, which is the
// state an applier-path write leaves behind. It is the fixture BUG-3000
// established; the predicate itself is proven in internal/store.
func makeStale(t *testing.T, srv *Server, itemID string) {
	t.Helper()
	if _, err := srv.store.AppendYjsUpdate(itemID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
}

// decodeMap reads a response body as a generic map so a key's ABSENCE is
// distinguishable from its zero value.
func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode response as map: %v\nbody: %s", err, body)
	}
	return m
}

// TestPlaybookRunCarriesTheMarkerBothWays covers the door BUG-3033 ranks first.
//
// A playbook body is EXECUTED, not merely read: a stale one is an agent running
// superseded steps. PlaybookRunResponse hoists four values out of the item into
// a hand-built shape, so it inherits nothing.
func TestPlaybookRunCarriesTheMarkerBothWays(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	pb := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship something",
		"content": "Step 1: do the thing",
		"fields":  `{"status":"active","invocation_slug":"ship"}`,
	})

	run := func(t *testing.T) map[string]any {
		t.Helper()
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/ship/run", map[string]any{})
		if rr.Code != http.StatusOK {
			t.Fatalf("run playbook: %d: %s", rr.Code, rr.Body.String())
		}
		return decodeMap(t, rr.Body.Bytes())
	}

	// ABSENCE FIRST.
	if got := run(t); got["content_state"] != nil {
		t.Fatalf("a current playbook's run response carries content_state=%v; "+
			"every assertion below would then prove nothing", got["content_state"])
	}

	makeStale(t, srv, pb.ID)

	got := run(t)
	if got["content_state"] != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("run content_state = %v, want %q — an agent is handed superseded steps with no signal",
			got["content_state"], models.ContentOutcomeAppliedPendingFlush)
	}
	// The body is still served. The marker qualifies the body; it does not
	// replace it, and a door that withheld the content would pass a
	// presence-only assertion.
	if body, _ := got["body"].(string); body == "" {
		t.Error("run response carries the marker but no body; the marker is a qualifier, not a substitute")
	}
}

// TestPlaybookShowInheritsTheContentStateMarker pins the SIBLING door.
//
// PlaybookShowResponse embeds *models.Item, so it gets the marker for free —
// which is exactly why it needs a test. Nothing in the type says the embed is
// load-bearing, and a future refactor to an explicit projection (the shape
// BootstrapCollection and BootstrapRole already took for size) would drop the
// field silently. Named in PlaybookRunResponse.ContentState's doc comment as the
// guard that keeps the two doors from diverging.
func TestPlaybookShowInheritsTheContentStateMarker(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	pb := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship something",
		"content": "Step 1: do the thing",
		"fields":  `{"status":"active","invocation_slug":"ship"}`,
	})

	show := func(t *testing.T) map[string]any {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/ship", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("show playbook: %d: %s", rr.Code, rr.Body.String())
		}
		return decodeMap(t, rr.Body.Bytes())
	}

	if got := show(t); got["content_state"] != nil {
		t.Fatalf("a current playbook's show response carries content_state=%v", got["content_state"])
	}

	makeStale(t, srv, pb.ID)

	if got := show(t); got["content_state"] != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("show content_state = %v, want %q — the embed that carries it has been broken",
			got["content_state"], models.ContentOutcomeAppliedPendingFlush)
	}
}

// TestBootstrapConventionBodiesCarryTheMarkerBothWays covers the second door an
// agent ACTS on: a stale convention is a rule followed after it changed.
func TestBootstrapConventionBodiesCarryTheMarkerBothWays(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	conv := createItem(t, srv, slug, "conventions", map[string]interface{}{
		"title":   "Always follow this",
		"content": "The rule an agent obeys.",
		"fields":  `{"status":"active","trigger":"always","priority":"must","scope":"all"}`,
	})

	// convention reads the bootstrap and returns the raw map for our convention,
	// so an absent key is distinguishable from an empty one.
	convention := func(t *testing.T) map[string]any {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("bootstrap: %d: %s", rr.Code, rr.Body.String())
		}
		blob := decodeMap(t, rr.Body.Bytes())
		list, _ := blob["conventions"].([]any)
		for _, e := range list {
			entry, _ := e.(map[string]any)
			if entry["ref"] == conv.Ref {
				return entry
			}
		}
		t.Fatalf("the convention did not come back in the bootstrap blob; this leg measured nothing")
		return nil
	}

	if got := convention(t); got["content_state"] != nil {
		t.Fatalf("a current convention carries content_state=%v in the bootstrap", got["content_state"])
	}

	makeStale(t, srv, conv.ID)

	got := convention(t)
	if got["content_state"] != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("bootstrap convention content_state = %v, want %q — an agent loads a superseded rule with no signal",
			got["content_state"], models.ContentOutcomeAppliedPendingFlush)
	}
	if body, _ := got["content"].(string); body == "" {
		t.Error("the convention carries the marker but no body")
	}
}

// TestBootstrapGenericIncludeBodiesCarryTheMarkerBothWays covers the door this
// unit's sweep found, which BUG-3033 does not name — the declaration-driven
// bodies-mode include group. Any collection can declare one, so the set of item
// bodies reaching an agent through here is open-ended in a way the fixed
// conventions payload is not.
//
// The third leg is the one worth reading: a METADATA-mode group must carry no
// marker even when the item is stale, because it carries no body. A marker on an
// absent body is a claim about something the group does not serve.
func TestBootstrapGenericIncludeBodiesCarryTheMarkerBothWays(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	mkColl := func(t *testing.T, name, key, mode string) {
		t.Helper()
		traits := `{"bootstrap_include":[{"mode":"` + mode + `","key":"` + key + `"}]}`
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name":   name,
			"schema": `{"fields":[{"key":"status","type":"select","options":["active"],"default":"active"}]}`,
			"traits": traits,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s collection: %d: %s", name, rr.Code, rr.Body.String())
		}
	}
	mkColl(t, "Runbooks", "runbooks", "bodies")
	mkColl(t, "Catalog", "catalog", "metadata")

	bodied := createItem(t, srv, slug, "runbooks", map[string]interface{}{
		"title":   "Restart the thing",
		"content": "Step 1: restart it",
		"fields":  `{"status":"active"}`,
	})
	meta := createItem(t, srv, slug, "catalog", map[string]interface{}{
		"title":   "Listed only",
		"content": "This body never ships in metadata mode.",
		"fields":  `{"status":"active"}`,
	})

	entry := func(t *testing.T, key, ref string) map[string]any {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("bootstrap: %d: %s", rr.Code, rr.Body.String())
		}
		blob := decodeMap(t, rr.Body.Bytes())
		groups, _ := blob["bootstrap_includes"].([]any)
		for _, g := range groups {
			group, _ := g.(map[string]any)
			if group["key"] != key {
				continue
			}
			items, _ := group["items"].([]any)
			for _, i := range items {
				it, _ := i.(map[string]any)
				if it["ref"] == ref {
					return it
				}
			}
		}
		t.Fatalf("no %q include entry for %s came back; this leg measured nothing", key, ref)
		return nil
	}

	if got := entry(t, "runbooks", bodied.Ref); got["content_state"] != nil {
		t.Fatalf("a current bodies-mode include carries content_state=%v", got["content_state"])
	}

	makeStale(t, srv, bodied.ID)
	makeStale(t, srv, meta.ID)

	got := entry(t, "runbooks", bodied.Ref)
	if got["content_state"] != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("bodies-mode include content_state = %v, want %q",
			got["content_state"], models.ContentOutcomeAppliedPendingFlush)
	}

	// METADATA mode: no body is served, so no claim about a body may be made —
	// even though this item is just as stale as the one above.
	metaEntry := entry(t, "catalog", meta.Ref)
	if metaEntry["content"] != nil {
		t.Fatalf("premise broken: the metadata-mode group served a body (%v), so the assertion below measures nothing",
			metaEntry["content"])
	}
	if metaEntry["content_state"] != nil {
		t.Errorf("a metadata-mode include marked its ABSENT body as stale (content_state=%v)",
			metaEntry["content_state"])
	}
}

// TestArtifactExportCarriesTheMarkerBothWays covers the portable-format door.
//
// The artifact is TEXT, so there is no struct to inherit the marker from — it has
// to be written into the format, and that is a compatibility question the JSON
// doors do not have. The legs here are the compatibility claims, made testable:
// a CURRENT item's bytes are byte-identical to what this format emitted before
// (no key, no blank line, no reordering), and a MARKED artifact still decodes
// with body and fields intact, so `pad item import` is unaffected.
func TestArtifactExportCarriesTheMarkerBothWays(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	const body = "Step 1: do the thing\n"
	pb := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship something",
		"content": body,
		"fields":  `{"status":"active","invocation_slug":"ship"}`,
	})

	export := func(t *testing.T) string {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+pb.Slug+"/export", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("export: %d: %s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	// ABSENCE FIRST — and on this door absence means the KEY IS NOT IN THE
	// FRONTMATTER AT ALL, which is what keeps a current export byte-identical.
	current := export(t)
	if strings.Contains(current, "content_state") {
		t.Fatalf("a current item's artifact mentions content_state; the format is no longer byte-identical:\n%s", current)
	}

	makeStale(t, srv, pb.ID)

	marked := export(t)
	if !strings.Contains(marked, "content_state: "+models.ContentOutcomeAppliedPendingFlush) {
		t.Errorf("a stale item's artifact carries no content_state:\n%s", marked)
	}

	// The marker rides in provenance, not beside the item's own keys: it is a
	// fact about the EXPORT, and a reader must not mistake it for a field.
	decoded, err := artifact.Decode([]byte(marked))
	if err != nil {
		t.Fatalf("a marked artifact no longer decodes — import is broken: %v", err)
	}
	if decoded.Provenance.ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("decoded provenance.content_state = %q, want %q",
			decoded.Provenance.ContentState, models.ContentOutcomeAppliedPendingFlush)
	}
	// The EXACT body, not merely a non-empty one. "Non-empty" is satisfied by an
	// export that replaced the body with unrelated text, which is the failure a
	// marker must never be able to hide (codex round 1 P3).
	if decoded.Body != body {
		t.Errorf("the marked artifact's body is not what the item holds:\n got: %q\nwant: %q", decoded.Body, body)
	}
	if decoded.Fields["invocation_slug"] != "ship" {
		t.Errorf("a marked artifact lost a field on decode: invocation_slug = %v", decoded.Fields["invocation_slug"])
	}
	// The current export must decode to the same body, so the comparison below
	// is between two artifacts of the same item rather than two arbitrary blobs.
	decodedCurrent, err := artifact.Decode([]byte(current))
	if err != nil {
		t.Fatalf("the current artifact does not decode: %v", err)
	}
	if decodedCurrent.Body != body {
		t.Errorf("the current artifact's body is not what the item holds:\n got: %q\nwant: %q",
			decodedCurrent.Body, body)
	}

	// Byte equality after removing the marker line and normalising the one field
	// that legitimately differs between two exports (exported_at is a
	// timestamp). A line-count check alone passes for any single-line change
	// anywhere in the document, including a rewritten body.
	normalise := func(doc string) string {
		out := make([]string, 0, 32)
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "content_state:") {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "exported_at:") {
				line = "  exported_at: <normalised>"
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	if got, want := normalise(marked), normalise(current); got != want {
		t.Errorf("the two exports differ by more than the marker line:\n marked: %q\ncurrent: %q", got, want)
	}
	// And the marker really was the only thing removed by that normalisation —
	// otherwise the comparison above could be passing because both sides were
	// stripped of something.
	if normalise(marked) == marked {
		t.Error("normalisation removed nothing from the marked export; the comparison above proved nothing")
	}
}

// TestPlaybookSummariesCarryTheMarkerBothWays covers the door codex round 1
// found, which this unit's own population table missed even though the
// precedent was already in the tree: BUG-3000 put the marker on
// cli.ItemSummary.ContentPreview on the grounds that "a content_preview is
// content", and a playbook `summary` is the identical shape — the body's first
// paragraph, truncated.
//
// Two doors, one projection: the bootstrap blob and the dedicated /playbooks
// endpoint both render collectPlaybookMetadata, so both are driven here. The
// third leg is the constraint that keeps the marker honest: a playbook with no
// summary makes no claim about one, even while its row is stale.
func TestPlaybookSummariesCarryTheMarkerBothWays(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	withBody := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship something",
		"content": "Ships a list of tasks end to end.",
		"fields":  `{"status":"active","invocation_slug":"ship"}`,
	})
	noBody := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Empty one",
		"content": "",
		"fields":  `{"status":"active","invocation_slug":"empty"}`,
	})

	// entries reads BOTH doors and returns the raw map for `ref` from each,
	// keyed by a label, so a key's absence is distinguishable from its zero
	// value and neither door can ride on the other's coverage.
	entries := func(t *testing.T, ref string) map[string]map[string]any {
		t.Helper()
		out := map[string]map[string]any{}

		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("bootstrap: %d: %s", rr.Code, rr.Body.String())
		}
		blob := decodeMap(t, rr.Body.Bytes())
		list, _ := blob["playbooks"].([]any)
		for _, e := range list {
			entry, _ := e.(map[string]any)
			if entry["ref"] == ref {
				out["bootstrap"] = entry
			}
		}

		rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("playbooks: %d: %s", rr.Code, rr.Body.String())
		}
		var raw []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode playbooks: %v", err)
		}
		for _, entry := range raw {
			if entry["ref"] == ref {
				out["playbooks endpoint"] = entry
			}
		}

		if len(out) != 2 {
			t.Fatalf("%s did not come back from both doors (got %d); this leg measured nothing", ref, len(out))
		}
		return out
	}

	// ABSENCE FIRST, on both doors.
	for door, entry := range entries(t, withBody.Ref) {
		if entry["summary"] == nil {
			t.Fatalf("%s: premise broken — no summary was emitted, so nothing below is about a summary", door)
		}
		if entry["content_state"] != nil {
			t.Fatalf("%s: a current playbook's summary is marked %v", door, entry["content_state"])
		}
	}

	makeStale(t, srv, withBody.ID)
	makeStale(t, srv, noBody.ID)

	for door, entry := range entries(t, withBody.Ref) {
		if entry["content_state"] != models.ContentOutcomeAppliedPendingFlush {
			t.Errorf("%s: summary content_state = %v, want %q — an agent routes on a description "+
				"taken from a body that has moved on", door, entry["content_state"],
				models.ContentOutcomeAppliedPendingFlush)
		}
	}

	// No summary, no claim — even though this item's row is just as stale.
	for door, entry := range entries(t, noBody.Ref) {
		if entry["summary"] != nil {
			t.Fatalf("%s: premise broken — the empty-bodied playbook emitted a summary (%v)",
				door, entry["summary"])
		}
		if entry["content_state"] != nil {
			t.Errorf("%s: a playbook with NO summary carries content_state=%v, claiming staleness "+
				"about something it does not serve", door, entry["content_state"])
		}
	}
}
