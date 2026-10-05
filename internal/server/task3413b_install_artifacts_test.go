package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3413 (SPEC-6 U9b): the install view lists the items the app's pack
// provisioned, so the owner can activate its drafts.

func TestTask3413b_TheInstallListsItsProvisionedItems(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	if len(p.Artifacts) == 0 {
		t.Fatal("precondition: the fixture app declares artifacts")
	}
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var conf appInstallConfirmResponse
	parseJSON(t, rr, &conf)

	get := func() appInstallStateResponse {
		t.Helper()
		rr := doRequestWithCookie(e.srv, "GET", "/api/v1/workspaces/"+e.ws+"/apps/"+conf.InstallID, nil, e.token)
		if rr.Code != http.StatusOK {
			t.Fatalf("get install: %d %s", rr.Code, rr.Body.String())
		}
		var out appInstallStateResponse
		parseJSON(t, rr, &out)
		return out
	}
	out := get()
	if len(out.Artifacts) != len(conf.Items) {
		t.Fatalf("artifacts = %+v, want the %d provisioned items %+v", out.Artifacts, len(conf.Items), conf.Items)
	}
	refs := map[string]bool{}
	for _, it := range conf.Items {
		refs[it.Ref] = true
	}
	for _, a := range out.Artifacts {
		if !refs[a.Ref] || a.Version != p.Version || a.Status != "draft" || a.Title == "" || a.CollectionSlug == "" {
			t.Errorf("artifact %+v; want a provisioned draft at version %s", a, p.Version)
		}
	}

	// A look-alike origin's stamp is not this install's, and a deleted item
	// is not listed.
	first := out.Artifacts[0]
	if _, err := e.srv.store.DB().Exec(`UPDATE items SET source_pack = ? WHERE id = ?`, e.origin()+".evil.example@9", first.ItemID); err != nil {
		t.Fatal(err)
	}
	if got := get().Artifacts; len(got) != len(out.Artifacts)-1 {
		t.Errorf("a look-alike origin's item was listed: %+v", got)
	}
	if _, err := e.srv.store.DB().Exec(`UPDATE items SET source_pack = ? WHERE id = ?`, e.origin()+"@"+p.Version, first.ItemID); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.store.DeleteItem(first.ItemID); err != nil {
		t.Fatal(err)
	}
	if got := get().Artifacts; len(got) != len(out.Artifacts)-1 {
		t.Errorf("a deleted item was listed: %+v", got)
	}
}

// The prefix is compared in characters: an origin whose host is not ASCII
// still finds its items (codex U9b r1).
func TestTask3413b_ANonASCIIOriginFindsItsItems(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	if rr := e.confirm(t, p, p.ManifestSHA256); rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d", rr.Code)
	}
	var id string
	if err := e.srv.store.DB().QueryRow(`SELECT id FROM items WHERE source_pack IS NOT NULL LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	const origin = "https://café.example"
	if _, err := e.srv.store.DB().Exec(`UPDATE items SET source_pack = ? WHERE id = ?`, origin+"@3.0.0", id); err != nil {
		t.Fatal(err)
	}
	got, err := e.srv.store.ListInstallArtifactItems(e.wsID, origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ItemID != id {
		t.Errorf("non-ASCII origin: %+v, want item %s", got, id)
	}
}

// §2 step 7 (lead ruling, U9b): activating an app's draft is an ordinary
// edit by a person, never the app's. An app token, service or a delegated
// owner's, cannot PATCH a draft playbook or convention the app's pack
// provisioned: those live in system collections, never companions, and app
// writes are companion-only (appWriteAllows).
func TestTask3413b_TheAppCannotActivateItsOwnDrafts(t *testing.T) {
	for _, kind := range []string{"service", "delegated owner"} {
		t.Run(kind, func(t *testing.T) {
			var f appAPIFix
			if kind == "service" {
				f = appAPIFixture(t, "write")
			} else {
				f = delegatedAPIFixture(t, "write", "write", "owner").appAPIFix
			}
			coll, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Playbooks Here", Slug: "playbooks-here",
				Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["draft","active"],"default":"draft"}]}`})
			if err != nil {
				t.Fatal(err)
			}
			var origin string
			if err := f.srv.store.DB().QueryRow(`SELECT origin FROM app_installs WHERE id = ?`, f.in.id).Scan(&origin); err != nil {
				t.Fatal(err)
			}
			item, err := f.srv.store.CreateItem(f.ws.ID, coll.ID, models.ItemCreate{Title: "Triage", Fields: `{"status":"draft"}`})
			if err != nil {
				t.Fatal(err)
			}
			// A system collection, as Playbooks and Conventions are.
			if _, err := f.srv.store.DB().Exec(`UPDATE collections SET is_system = 1 WHERE id = ?`, coll.ID); err != nil {
				t.Fatal(err)
			}
			// Stamped as provisioning stamps an app's artifact.
			if _, err := f.srv.store.DB().Exec(`UPDATE items SET source_pack = ? WHERE id = ?`, origin+"@1.0.0", item.ID); err != nil {
				t.Fatal(err)
			}
			// A valid etag, so a refusal is the write rule, not a precondition.
			etag := f.etag(t, item.ID)
			rr := appDo(f.srv, "PATCH", f.path("/items/"+item.ID), f.token, map[string]any{"fields_patch": map[string]any{"status": "active"}, "expected_etag": etag})
			if rr.Code != http.StatusForbidden {
				t.Errorf("app PATCH of its draft to active: %d %s, want 403", rr.Code, rr.Body.String())
			}
			got, err := f.srv.store.GetItem(item.ID)
			if err != nil || got == nil {
				t.Fatal(err)
			}
			if itemStatus(got) != "draft" {
				t.Errorf("status = %q after the app's attempt, want draft", itemStatus(got))
			}
		})
	}
}
