package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSearchCommentResponseAndPermission(t *testing.T) {
	t.Parallel()
	e := bug3331Setup(t)
	items, err := e.srv.store.ListItems(e.wsID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	commentIDs := map[string]string{}
	for _, item := range items {
		c, err := e.srv.store.CreateComment(e.wsID, item.ID, "", models.CommentCreate{Body: "nebulaquartz comment context"})
		if err != nil {
			t.Fatal(err)
		}
		commentIDs[item.Title] = c.ID
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
			CommentID string `json:"comment_id"`
			Snippet   string `json:"snippet"`
		} `json:"results"`
		Total  int `json:"total"`
		Facets struct {
			Collections map[string]int `json:"collections"`
		} `json:"facets"`
	}
	if err := json.Unmarshal([]byte(rr.body), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || len(response.Results) != 1 {
		t.Fatalf("search: %s", rr.body)
	}
	hit := response.Results[0]
	if hit.Item.Title != "Zebra alpha" || hit.MatchedIn != "comment" || hit.CommentID != commentIDs["Zebra alpha"] || hit.Snippet == "" {
		t.Errorf("wrong source: %+v", hit)
	}
	if response.Facets.Collections["docs"] != 1 || len(response.Facets.Collections) != 1 {
		t.Errorf("facets: %+v", response.Facets)
	}
}
