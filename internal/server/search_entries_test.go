package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4491: a note or decision match carries matched_in and entry_id on the
// wire, and only for an item the caller may see.
func TestSearchEntryResponseAndPermission(t *testing.T) {
	t.Parallel()
	e := bug3331Setup(t)
	items, err := e.srv.store.ListItems(e.wsID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	entryIDs := map[string]string{}
	for i, item := range items {
		id := "note-wire-" + item.ID
		update := models.ItemUpdate{ImplementationNoteToAppend: &models.ItemImplementationNote{ID: id, Summary: "Wired", Details: "nebulaquartz entry context"}}
		if i%2 == 1 {
			id = "decision-wire-" + item.ID
			update = models.ItemUpdate{DecisionToAppend: &models.ItemDecisionLogEntry{ID: id, Decision: "Wired", Rationale: "nebulaquartz entry context"}}
		}
		if _, err := e.srv.store.UpdateItem(item.ID, update); err != nil {
			t.Fatal(err)
		}
		entryIDs[item.Title] = id
	}
	path := "/api/v1/search?" + url.Values{"q": {"nebulaquartz"}, "workspace": {e.wsSlug}}.Encode()
	rr := e.asCookie("guestitem")(path)
	if rr.code != http.StatusOK {
		t.Fatalf("search: %d %s", rr.code, rr.body)
	}
	var response struct {
		Results []struct {
			Item struct {
				Title string `json:"title"`
			} `json:"item"`
			MatchedIn string `json:"matched_in"`
			EntryID   string `json:"entry_id"`
			Snippet   string `json:"snippet"`
		} `json:"results"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(rr.body), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || len(response.Results) != 1 {
		t.Fatalf("search: %s", rr.body)
	}
	hit := response.Results[0]
	want := "note"
	if strings.HasPrefix(entryIDs["Zebra alpha"], "decision-") {
		want = "decision"
	}
	if hit.Item.Title != "Zebra alpha" || hit.MatchedIn != want || hit.EntryID != entryIDs["Zebra alpha"] || !strings.Contains(hit.Snippet, "<mark>") {
		t.Errorf("wrong source: %+v", hit)
	}
	for title, id := range entryIDs {
		if title != "Zebra alpha" && strings.Contains(rr.body, id) {
			t.Errorf("the response names %s's entry %s", title, id)
		}
	}
}
