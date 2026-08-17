package server

// BUG-2615 end to end. TestImportBundle_RoundTrip proves the remap for a
// reference living in ITEM CONTENT. The remap walked items only, so a bundle
// whose reference lives in a COMMENT imported with the source workspace's id
// intact — a reference that resolves to nothing in the destination, while the
// freshly-rehydrated attachment row it should point at ends up referenced by
// nothing and eventually reclaimed by the orphan GC.
//
// Driven through the real export → import path rather than the store helper,
// because that is where the bug was reachable and because the filing asked for
// exactly this fixture: a bundle whose ONLY reference to an attachment lives
// in a comment.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestImportBundle_RemapsCommentOnlyAttachmentRefs(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)

	body := realPNG()
	rr := doMultipartUpload(src, srcSlug, "in-comment.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var upload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &upload); err != nil {
		t.Fatalf("decode upload: %v", err)
	}

	// The item deliberately carries NO reference. If it did, the items walk
	// alone would remap the id and this test would pass on the unfixed code —
	// the reference has to live ONLY in the comment for the fixture to arm.
	rr = doRequest(src, "POST", "/api/v1/workspaces/"+srcSlug+"/collections/docs/items",
		map[string]any{"title": "Comment Carrier", "content": "no reference in this body\n"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode item: %v", err)
	}

	commentBody := fmt.Sprintf("see ![shot](pad-attachment:%s) for context\n", upload.ID)
	rr = doRequest(src, "POST",
		"/api/v1/workspaces/"+srcSlug+"/items/"+item.Slug+"/comments",
		map[string]any{"body": commentBody, "author": "alice", "source": "web"})
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create comment: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	bundle := rr.Body.Bytes()

	// A separate server, as the round-trip test does: a same-server import can
	// pass with the remap broken, because the old ids still resolve there.
	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=CommentImported",
		bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var newWS models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &newWS); err != nil {
		t.Fatalf("decode new ws: %v", err)
	}

	rr = doRequest(dest, "GET", "/api/v1/workspaces/"+newWS.Slug+"/attachments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list attachments: %d %s", rr.Code, rr.Body.String())
	}
	var attResp struct {
		Attachments []struct {
			ID string `json:"id"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &attResp); err != nil {
		t.Fatalf("decode att list: %v", err)
	}
	if len(attResp.Attachments) != 1 {
		t.Fatalf("imported attachments: got %d, want 1", len(attResp.Attachments))
	}
	newAttID := attResp.Attachments[0].ID
	if newAttID == upload.ID {
		t.Fatalf("attachment id was not remapped at all (got %s) — the fixture proves "+
			"nothing about comment bodies if the import reused the source id", newAttID)
	}

	// Find the imported item, then read its comments.
	rr = doRequest(dest, "GET", "/api/v1/workspaces/"+newWS.Slug+"/collections/docs/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items: %d %s", rr.Code, rr.Body.String())
	}
	var items []models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	var imported *models.Item
	for i := range items {
		if items[i].Title == "Comment Carrier" {
			imported = &items[i]
			break
		}
	}
	if imported == nil {
		t.Fatalf("imported item not found; got %d items", len(items))
	}

	rr = doRequest(dest, "GET",
		"/api/v1/workspaces/"+newWS.Slug+"/items/"+imported.Slug+"/comments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list comments: %d %s", rr.Code, rr.Body.String())
	}
	var comments []models.Comment
	if err := json.Unmarshal(rr.Body.Bytes(), &comments); err != nil {
		t.Fatalf("decode comments: %v body=%s", err, rr.Body.String())
	}
	if len(comments) != 1 {
		t.Fatalf("imported comments: got %d, want 1 — without the comment the rest "+
			"of this test asserts nothing", len(comments))
	}

	if !strings.Contains(comments[0].Body, "pad-attachment:"+newAttID) {
		t.Errorf("imported comment does not reference the rehydrated attachment %s; body=%q",
			newAttID, comments[0].Body)
	}
	if strings.Contains(comments[0].Body, "pad-attachment:"+upload.ID) {
		t.Errorf("imported comment still carries the SOURCE workspace's id %s; body=%q",
			upload.ID, comments[0].Body)
	}
}
