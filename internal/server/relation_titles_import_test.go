package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestArtifactImport_DropsFieldKeysTheKindDoesNotDeclare pins the assumption
// that makes the artifact door's relation-visibility question MOOT
// (PLAN-2857 U6, codex round 6 P1).
//
// Round 6 raised a real-sounding P1: handleImportArtifact uses the
// `relationsCarry` posture, so a carried relation title would be judged by the
// IMPORTER's visibility and two people importing one artifact would store
// different bytes. The lead ruled that import KEEPS visibility — an import has
// no prior truth, and exempting it would let a crafted title resolve to a UUID
// the importer cannot see but can then read straight back off their own item.
//
// The reasoning is sound and the path does not exist. An artifact's frontmatter
// carries a FIXED ALLOW-LIST of field keys per kind, and anything else is
// dropped at DECODE, before any relation code runs — so no relation value can
// ride in an artifact, whatever the destination collection's schema has been
// reshaped to.
//
// I wrote the ruled test first and it passed while asserting nothing: the field
// never arrived, so the leg could not tell the visibility rule from its own
// absence, and a mutation removing the predicate entirely SURVIVED it.
//
// WHAT THIS TEST DOES AND DOES NOT ESTABLISH, because the same trap is easy to
// fall into twice. It asserts the OBSERVABLE invariant end to end: a
// frontmatter key the kind does not declare never reaches the stored blob, even
// when the destination schema declares it as a relation and the artifact bytes
// really carry it (they are hand-built below, because artifact.Encode filters
// on the way OUT and an Encode-built fixture never puts the key on the wire).
// The control asserts declared keys DO land, so it is not passing by storing
// nothing.
//
// It does NOT identify which layer drops the key, and it is not a guard on the
// decode allow-list specifically: widening playbookFieldKeys does not make this
// test fail, so at least one further filter sits behind it that I did not
// isolate. The claim is "no relation value reaches the import door", verified;
// not "here is the single mechanism that stops it".
func TestArtifactImport_DropsFieldKeysTheKindDoesNotDeclare(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", wsSlug, err)
	}

	// Reshape the destination so the schema WOULD accept the key. If the
	// artifact format ever stops filtering, this is what lets the value land.
	secrets := mustSchemaCollection(t, srv, ws.ID, "Secrets", `{"fields":[]}`)
	if _, err := srv.store.CreateItem(ws.ID, secrets.ID, models.ItemCreate{Title: "Hidden Owner"}); err != nil {
		t.Fatalf("CreateItem(target): %v", err)
	}
	colls, err := srv.store.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	var playbooks *models.Collection
	for i := range colls {
		if colls[i].Slug == "playbooks" {
			playbooks = &colls[i]
		}
	}
	if playbooks == nil {
		t.Fatalf("no playbooks collection in the seeded workspace (have %d)", len(colls))
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(playbooks.Schema), &schema); err != nil {
		t.Fatalf("decode playbooks schema: %v", err)
	}
	fields, _ := schema["fields"].([]any)
	schema["fields"] = append(fields, map[string]any{
		"key": "owner_ref", "label": "Owner", "type": "relation", "collection": secrets.Slug,
	})
	reshaped, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("encode schema: %v", err)
	}
	reshapedStr := string(reshaped)
	if _, err := srv.store.UpdateCollection(playbooks.ID, models.CollectionUpdate{Schema: &reshapedStr}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}

	// The reshape must actually have taken, or the assertion below passes
	// because the field is undeclared rather than because the door filters it.
	reread, rerr := srv.store.GetCollection(playbooks.ID)
	if rerr != nil {
		t.Fatalf("re-read playbooks: %v", rerr)
	}
	if !strings.Contains(reread.Schema, "owner_ref") {
		t.Fatalf("the schema reshape did not take, so this test would prove nothing: %s", reread.Schema)
	}

	// HAND-BUILT BYTES, not artifact.Encode. Encode writes only the kind's
	// known keys into frontmatter, so an artifact built through it never
	// carries the extra key and the decode-side filter this test is about is
	// never exercised — my first version did exactly that and a mutation
	// widening the decode allow-list SURVIVED it.
	data := []byte("---\n" +
		"pad_artifact: playbook\n" +
		fmt.Sprintf("format_version: %v\n", artifact.FormatVersion) +
		"title: Artifact With An Undeclared Key\n" +
		"status: active\n" +
		"trigger: manual\n" +
		"scope: all\n" +
		"owner_ref: Hidden Owner\n" +
		"---\n\nbody\n")

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rr.Body.String())
	}
	stored, err := srv.store.GetItemBySlug(ws.ID, envelope.Slug)
	if err != nil || stored == nil {
		t.Fatalf("GetItemBySlug(%s): %v", envelope.Slug, err)
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(stored.Fields), &blob); err != nil {
		t.Fatalf("decode fields: %v", err)
	}

	if v, present := blob["owner_ref"]; present {
		t.Errorf("the artifact door carried %q = %#v into a RELATION field. The frontmatter allow-list used to drop it before any relation code ran, which is why PLAN-2857 U6's visibility rule was never implemented for this door. It is now reachable and the lead's ruling needs applying there", "owner_ref", v)
	}
	// CONTROL: the DECLARED keys did land, so the assertion above is about the
	// allow-list filtering rather than about the import storing nothing.
	if blob["trigger"] != "manual" || blob["scope"] != "all" {
		t.Errorf("declared frontmatter keys did not land (%#v) — the leg above proves nothing without this", blob)
	}
}
