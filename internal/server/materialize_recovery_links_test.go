package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2198 U4, legs (i) and (ii) of the link-index ruling. The recovery
// converts internal links against the WHOLE workspace's index (every live
// item, no viewer), so the question is whether that index can put text into a
// recovered body — above all an item's TITLE, which a restricted viewer of the
// body may not be entitled to see.
//
// The documents are in testdata/materialize_links.json, written by
// web/src/lib/collab/materializer/materializerLinkFixture.test.ts: each is a
// real Y.Doc holding a link as a tab stores it after loading. `raw` is the
// same document through the flush pipeline with NO index, i.e. the text the
// document itself contains.

type linkFixtureCase struct {
	Name     string   `json:"name"`
	Markdown string   `json:"markdown"`
	Rows     []string `json:"rows"`
	Raw      string   `json:"raw"`
}

func loadLinkFixture(t *testing.T) []linkFixtureCase {
	t.Helper()
	b, err := os.ReadFile("testdata/materialize_links.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []linkFixtureCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("fixture has %d cases", len(cases))
	}
	return cases
}

const secretTitle = "Secret Launch Codename"

// linkWorld: workspace "ws" holding X = SECR-1 (secretTitle) in "secrets", a
// collection a restricted member or guest would not see, and PUB-2 in
// "public". Item numbers are workspace-sequential, so the order of creation
// fixes both refs; the test checks them.
func newLinkWorld(t *testing.T) (*recoveryFixture, *models.Item, *models.Item) {
	t.Helper()
	f := newRecoveryFixture(t, recoveryRunner(t), time.Minute, materializeRecoveryConfig{})
	secrets, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Secrets", Prefix: "SECR", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	public, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Public", Prefix: "PUB", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	x, err := f.srv.store.CreateItem(f.ws.ID, secrets.ID, models.ItemCreate{Title: secretTitle, Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.srv.store.CreateItem(f.ws.ID, public.ID, models.ItemCreate{Title: "Public Plan", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	x, _ = f.srv.store.GetItem(x.ID)
	p, _ = f.srv.store.GetItem(p.ID)
	if x.Ref != "SECR-1" || x.Slug != "secret-launch-codename" || x.CollectionSlug != "secrets" || p.Ref != "PUB-2" {
		t.Fatalf("fixture refs: X %s/%s/%s, P %s", x.Ref, x.Slug, x.CollectionSlug, p.Ref)
	}
	return f, x, p
}

// recoverCase inserts the case's op-log over a stale body and runs one
// recovery pass through the production path (index builder included).
func recoverCase(t *testing.T, f *recoveryFixture, c linkFixtureCase) string {
	t.Helper()
	it := f.item(t, "Recovering "+c.Name, "stale")
	f.appendRows(t, it.ID, decodeRows(t, c.Rows)...)
	if res := f.r.process(it.ID); res != mrApplied {
		t.Fatalf("%s: process = %q", c.Name, res)
	}
	got, _ := f.srv.store.GetItem(it.ID)
	return got.Content
}

// LEG (i). The STOP condition: no index-sourced TITLE may appear in a
// recovered body. Each form markdownToWikiLinks can emit is covered:
//
//	[[REF]]          link text == the item's CURRENT title: the title was in
//	                 the document and is REMOVED, not added.
//	[[REF|text]]     any other text: `text` comes from the document.
//	[[Title]]        the legacy form for an item with no ref: `text` again.
//	                 (Every item the store mints has a ref, so the legacy
//	                 branch is reached only for rows with a NULL item_number;
//	                 it emits the document's link text either way.)
//	[[ws::REF|text]] cross-workspace: the index is not consulted.
//
// What the index CAN contribute is the REF, and only for a link addressed by
// SLUG (`/owner/ws/coll/<slug>` becomes `[[SECR-1|text]]`). The slug itself is
// in the document; the ref is a number the item already exposes in its URL
// wherever it is linked by ref. That is the whole of the index's contribution.
func TestMaterializeLinkIndexContributesNoTitle(t *testing.T) {
	f, _, _ := newLinkWorld(t)
	index, err := f.srv.materializeLinkIndex(f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"ref-link-other-text":       "See [[SECR-1|shown text]] here.",
		"ref-link-old-title":        "See [[SECR-1|Old Codename]] here.",
		"slug-link-slug-text":       "See [[SECR-1|the plan]] here.",
		"text-equals-current-title": "See [[SECR-1]] here.",
		"broken-link":               "See [[gone text]] here.",
		"cross-workspace":           "See [[other::SECR-1|xw text]] here.",
		"typed-wiki-literal":        "Typed [[SECR-1]] literal.",
		"public-ref-link":           "See [[PUB-2|the public plan]] here.",
	}
	for _, c := range loadLinkFixture(t) {
		got := recoverCase(t, f, c)
		if w, ok := want[c.Name]; !ok || got != w {
			t.Errorf("%s: recovered %q, want %q", c.Name, got, w)
		}
		// The general claim, over EVERY title in the index the job was given:
		// a title in the output must already be in the document.
		for _, e := range index {
			if e.Title != "" && strings.Contains(got, e.Title) && !strings.Contains(c.Raw, e.Title) {
				t.Errorf("STOP: %s: title %q of %s-%d is in the recovered body but not in the document\nbody: %q\ndoc:  %q",
					c.Name, e.Title, e.CollectionPrefix, e.ItemNumber, got, c.Raw)
			}
		}
		if strings.Contains(got, secretTitle) {
			t.Errorf("STOP: %s: X's title reached the recovered body: %q", c.Name, got)
		}
	}
}

// LEG (ii). A link to an item a restricted viewer cannot see survives the
// recovery as the same [[REF|text]] a full member's tab would store, because
// the index is the whole workspace. With a VIEWER-RESTRICTED index (what a
// restricted tab's localIndex holds) the same document stores a raw URL
// instead, bound to the owner and workspace slugs, which is the degradation
// the whole-workspace ruling exists to prevent.
func TestMaterializeLinkToRestrictedItemSurvives(t *testing.T) {
	f, x, _ := newLinkWorld(t)
	var c linkFixtureCase
	for _, k := range loadLinkFixture(t) {
		if k.Name == "ref-link-other-text" {
			c = k
		}
	}
	got := recoverCase(t, f, c)
	if got != "See [[SECR-1|shown text]] here." {
		t.Fatalf("recovered %q: the link to the restricted item did not survive as [[SECR-1|shown text]]", got)
	}

	// The counterfactual, through the same bundle: drop X's collection.
	full, err := f.srv.materializeLinkIndex(f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	var restricted []materialize.LinkEntry
	for _, e := range full {
		if e.CollectionPrefix != "SECR" {
			restricted = append(restricted, e)
		}
	}
	if len(restricted) != len(full)-1 {
		t.Fatalf("restricted index %d of %d; X %s not singled out", len(restricted), len(full), x.Ref)
	}
	md, err := recoveryRunner(t).Materialize(context.Background(), materialize.Job{
		Rows: decodeRows(t, c.Rows), SchemaVersion: collab.DefaultSchemaVersion, LinkIndex: restricted, WorkspaceSlug: "ws",
	})
	if err != nil {
		t.Fatal(err)
	}
	if md != "See [shown text](/alice/ws/secrets/SECR-1) here." {
		t.Fatalf("restricted-index counterfactual = %q", md)
	}
}

// The index is every live item, ordered as localIndex.getAll orders a tab's:
// updated_at DESC, then id ASC.
func TestMaterializeLinkIndexOrderAndScope(t *testing.T) {
	f, x, p := newLinkWorld(t)
	gone := f.item(t, "Archived one", "")
	if err := f.srv.store.DeleteItem(gone.ID); err != nil {
		t.Fatal(err)
	}
	// Make X the most recently updated.
	title := secretTitle
	if _, err := f.srv.store.UpdateItem(x.ID, models.ItemUpdate{Title: &title, Content: strPtr("touched")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // updated_at has one-second resolution
	body := "later"
	if _, err := f.srv.store.UpdateItem(p.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatal(err)
	}
	index, err := f.srv.materializeLinkIndex(f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(index) != 2 {
		t.Fatalf("index = %+v, want the two live items (archived excluded)", index)
	}
	if index[0].Title != "Public Plan" || index[1].Title != secretTitle {
		t.Fatalf("order = [%q, %q], want most recently updated first", index[0].Title, index[1].Title)
	}
	if index[1].ItemNumber != 1 || index[1].CollectionPrefix != "SECR" || index[1].Slug != "secret-launch-codename" {
		t.Fatalf("entry = %+v", index[1])
	}
}

func strPtr(s string) *string { return &s }
