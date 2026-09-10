package mcp

// PLAN-2857 U6 — a relation value survives the REMOTE transport in both
// directions: an exact TITLE written through it resolves to the canonical id,
// and the hydrated `relation_targets` reaches the agent through the summary
// projection.
//
// WHY THIS IS NOT CEREMONIAL, given the server doors are already tested. The
// remote list path does not forward the handler's response — it re-projects it
// through cli.ToItemSummaries, and that projection drops UUID plumbing on
// purpose. relation_targets was dropped by it during this very unit, which is
// exactly the failure a handler-level test cannot see: the server was correct
// and the agent still received a bare id with nothing to render it. Same
// rationale as the summary-shape tests above, and the same fixture style —
// real server, real store, because a stubbed handler can encode a response the
// endpoint never produces.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

type relationTargetsFixture struct {
	d      *HTTPHandlerDispatcher
	store  *store.Store
	ws     *models.Workspace
	cars   *models.Collection
	target *models.Item
}

func newRelationTargetsFixture(t *testing.T) *relationTargetsFixture {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{
		Email: "rel-owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Rel WS", Slug: "rel-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	colors, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Colors", Slug: "colors", Prefix: "COLO",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection(colors): %v", err)
	}
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Cars", Slug: "cars", Prefix: "CAR",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"relation","collection":"colors"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection(cars): %v", err)
	}
	red, err := s.CreateItem(ws.ID, colors.ID, models.ItemCreate{Title: "Red", CreatedBy: owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(red): %v", err)
	}

	return &relationTargetsFixture{
		d: &HTTPHandlerDispatcher{
			Handler:      srv,
			UserResolver: func(context.Context) *models.User { return owner },
		},
		store: s, ws: ws, cars: cars, target: red,
	}
}

// TestHTTPRelation_TitleWrittenOverTheRemoteDoorStoresTheCanonicalID: the write
// half. The title is resolved SERVER-side, so the remote door must carry the
// caller's string through untouched and store what the server resolved.
func TestHTTPRelation_TitleWrittenOverTheRemoteDoorStoresTheCanonicalID(t *testing.T) {
	f := newRelationTargetsFixture(t)

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "rel-ws",
		"collection": "cars",
		"title":      "By title",
		"field":      []any{"color=Red"},
	})
	res, err := f.d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch(item create): %v", err)
	}
	if res.IsError {
		t.Fatalf("create refused an exact title: %s", textOf(res))
	}

	// Read the STORED value back, not the response: a door that echoed the
	// right thing while writing the wrong thing would pass a response check.
	items, err := f.store.ListItems(f.ws.ID, models.ItemListParams{CollectionSlug: "cars"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 car, got %d", len(items))
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(items[0].Fields), &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["color"] != f.target.ID {
		t.Errorf("stored color = %v, want the canonical id %s — the title did not resolve through this transport", fields["color"], f.target.ID)
	}
}

// TestHTTPRelation_ListCarriesHydratedTargets: the read half, and the one that
// caught a real strip. The summary projection must keep relation_targets.
func TestHTTPRelation_ListCarriesHydratedTargets(t *testing.T) {
	f := newRelationTargetsFixture(t)

	if _, err := f.store.CreateItem(f.ws.ID, f.cars.ID, models.ItemCreate{
		Title:  "Ruby car",
		Fields: `{"status":"open","color":"` + f.target.ID + `"}`,
	}); err != nil {
		t.Fatalf("CreateItem(car): %v", err)
	}

	text := dispatchList(t, f.d, map[string]any{"workspace": "rel-ws", "collection": "cars"})

	var items []struct {
		Ref             string `json:"ref"`
		RelationTargets map[string]struct {
			ID    string `json:"id"`
			Ref   string `json:"ref"`
			Title string `json:"title"`
		} `json:"relation_targets"`
	}
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		t.Fatalf("decode summaries: %v\n%s", err, text)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d\n%s", len(items), text)
	}
	got, ok := items[0].RelationTargets["color"]
	if !ok {
		t.Fatalf("the summary projection dropped relation_targets, so an agent gets a bare id with nothing to render it — the exact defect U6 exists to close:\n%s", text)
	}
	if got.ID != f.target.ID || got.Ref != f.target.Ref || got.Title != "Red" {
		t.Errorf("hydrated as %+v, want {id:%s ref:%s title:Red}", got, f.target.ID, f.target.Ref)
	}
}
