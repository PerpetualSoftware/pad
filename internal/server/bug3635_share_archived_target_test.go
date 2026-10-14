package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3635: a public share link must serve nothing once its target is no
// longer live. GetItem checks only the item's own deleted_at, so before the
// fix an item share whose collection was archived kept serving the item's
// content and fields, and the attachment endpoint kept serving thumbnail
// bytes for an archived item and for an archived collection. Each test ends
// with a restore control: un-archiving serves again, so the 404 is the
// target's liveness and not some other refusal.

func (f shareAssetFixture) resolveStatus(token string) int {
	return doRequest(f.srv, "GET", "/api/v1/s/"+token, nil).Code
}

func (f shareAssetFixture) archiveCollection(t *testing.T) {
	t.Helper()
	if err := f.srv.store.DeleteCollection(f.collID, ""); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}
}

func (f shareAssetFixture) restoreCollection(t *testing.T) {
	t.Helper()
	if _, err := f.srv.store.RestoreCollection(f.wsID, f.collID); err != nil {
		t.Fatalf("RestoreCollection: %v", err)
	}
}

func TestBUG3635_ItemShareInArchivedCollection(t *testing.T) {
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, nil)

	if code := f.resolveStatus(link.Token); code != http.StatusOK {
		t.Fatalf("precondition: live item share resolve status %d, want 200", code)
	}
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Fatalf("precondition: live item share asset status %d, want 200", rr.Code)
	}

	f.archiveCollection(t)
	if code := f.resolveStatus(link.Token); code != http.StatusNotFound {
		t.Errorf("item share in an archived collection: resolve status %d, want 404", code)
	}
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Errorf("item share in an archived collection: asset status %d, want 404", rr.Code)
	}

	f.restoreCollection(t)
	if code := f.resolveStatus(link.Token); code != http.StatusOK {
		t.Errorf("restored collection: resolve status %d, want 200", code)
	}
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Errorf("restored collection: asset status %d, want 200", rr.Code)
	}
}

func TestBUG3635_ItemShareOfArchivedItem(t *testing.T) {
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x21)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, nil)

	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Fatalf("precondition: live item share asset status %d, want 200", rr.Code)
	}

	if err := f.srv.store.DeleteItem(f.itemID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if code := f.resolveStatus(link.Token); code != http.StatusNotFound {
		t.Errorf("archived item: resolve status %d, want 404", code)
	}
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Errorf("archived item: asset status %d, want 404", rr.Code)
	}

	if _, err := f.srv.store.RestoreItem(f.itemID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Errorf("restored item: asset status %d, want 200", rr.Code)
	}
}

func TestBUG3635_CollectionShareOfArchivedCollection(t *testing.T) {
	t.Parallel()
	f := newShareAssetFixture(t)
	a2, _ := f.seedImage(t, f.item2ID, 0x31)
	f.setContent(t, f.item2ID, imageRef(a2))
	link := f.createLink(t, "collection", f.collID, nil)

	if rr := f.getAsset(link.Token, a2, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Fatalf("precondition: live collection share asset status %d, want 200", rr.Code)
	}

	f.archiveCollection(t)
	if code := f.resolveStatus(link.Token); code != http.StatusNotFound {
		t.Errorf("archived collection: resolve status %d, want 404", code)
	}
	if rr := f.getAsset(link.Token, a2, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Errorf("archived collection: asset status %d, want 404", rr.Code)
	}

	f.restoreCollection(t)
	if code := f.resolveStatus(link.Token); code != http.StatusOK {
		t.Errorf("restored collection: resolve status %d, want 200", code)
	}
	if rr := f.getAsset(link.Token, a2, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Errorf("restored collection: asset status %d, want 200", rr.Code)
	}
}

func TestBUG3635_CollectionShareOfArchivedItem(t *testing.T) {
	t.Parallel()
	f := newShareAssetFixture(t)
	a2, _ := f.seedImage(t, f.item2ID, 0x41)
	f.setContent(t, f.item2ID, imageRef(a2))
	link := f.createLink(t, "collection", f.collID, nil)

	if body := doRequest(f.srv, "GET", "/api/v1/s/"+link.Token, nil).Body.String(); !strings.Contains(body, `"title":"Other"`) {
		t.Fatalf("precondition: live collection share JSON lacks the item: %s", body)
	}
	if err := f.srv.store.DeleteItem(f.item2ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	rr := doRequest(f.srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), `"title":"Other"`) {
		t.Errorf("collection share JSON after archiving an item: status %d, body %s; want 200 without it", rr.Code, rr.Body.String())
	}
	if rr := f.getAsset(link.Token, a2, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Errorf("collection share, archived item: asset status %d, want 404", rr.Code)
	}
	if _, err := f.srv.store.RestoreItem(f.item2ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if rr := f.getAsset(link.Token, a2, "variant=thumb-md"); rr.Code != http.StatusOK {
		t.Errorf("collection share, restored item: asset status %d, want 200", rr.Code)
	}
}

func TestBUG3635_ArchivedTargetDoesNotSpendMaxViews(t *testing.T) {
	// A request refused because the target is archived must not charge a
	// view: under max_views=1 the restored link still serves once.
	t.Parallel()
	for _, tc := range []struct{ name, targetType string }{
		{"item", "item"},
		{"collection", "collection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newShareAssetFixture(t)
			target := f.itemID
			if tc.targetType == "collection" {
				target = f.collID
			}
			one := 1
			link := f.createLink(t, tc.targetType, target, &store.ShareLinkOptions{MaxViews: &one})

			f.archiveCollection(t)
			if code := f.resolveStatus(link.Token); code != http.StatusNotFound {
				t.Fatalf("archived target: resolve status %d, want 404", code)
			}
			f.restoreCollection(t)
			if code := f.resolveStatus(link.Token); code != http.StatusOK {
				t.Errorf("restored target under max_views=1: resolve status %d, want 200 (the refused request spent the view)", code)
			}
		})
	}
}
