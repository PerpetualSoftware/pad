package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Tests for the share-link asset byte path (BUG-2389 2b / TASK-2637):
// GET /api/v1/s/{token}/attachments/{id}, variants-only, plus the signed-ref
// gate for protected links. The 7 pins from the TASK-2637 design forks live
// here, each written so removing the guard it targets turns it red.

// --- signing unit tests -------------------------------------------------

func TestShareAssetSig_RoundTrip(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	exp := time.Now().Add(time.Minute).Unix()
	sig := signShareAsset(secret, "link-1", "att-1", exp)
	if sig == "" {
		t.Fatal("signShareAsset returned empty with a valid secret")
	}
	if !verifyShareAsset(secret, "link-1", "att-1", strconv.FormatInt(exp, 10), sig, time.Now()) {
		t.Fatal("verifyShareAsset rejected a signature it just minted")
	}
}

func TestShareAssetSig_ScopedToAttachment(t *testing.T) {
	// fork-2 pin #4 (unit level): a signature minted for attachment X must not
	// validate for attachment Y under the same link + expiry.
	t.Parallel()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	exp := time.Now().Add(time.Minute).Unix()
	sigX := signShareAsset(secret, "link-1", "att-X", exp)
	if verifyShareAsset(secret, "link-1", "att-Y", strconv.FormatInt(exp, 10), sigX, time.Now()) {
		t.Fatal("signature for att-X validated for att-Y — scope binding broken")
	}
}

func TestShareAssetSig_ScopedToLink(t *testing.T) {
	// A signature minted under link A must not validate under link B (a token
	// swap). Guards against a leaked sig being replayed on a different share.
	t.Parallel()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	exp := time.Now().Add(time.Minute).Unix()
	sigA := signShareAsset(secret, "link-A", "att-1", exp)
	if verifyShareAsset(secret, "link-B", "att-1", strconv.FormatInt(exp, 10), sigA, time.Now()) {
		t.Fatal("signature for link-A validated under link-B — link binding broken")
	}
}

func TestShareAssetSig_Expired(t *testing.T) {
	// fork-2 pin #3 (unit level): a signature past its exp is rejected.
	t.Parallel()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	exp := time.Now().Add(-time.Second).Unix()
	sig := signShareAsset(secret, "link-1", "att-1", exp)
	if verifyShareAsset(secret, "link-1", "att-1", strconv.FormatInt(exp, 10), sig, time.Now()) {
		t.Fatal("an expired signature validated")
	}
}

func TestShareAssetSig_ExpTampered(t *testing.T) {
	// Extending exp without re-signing must fail — exp is inside the MAC.
	t.Parallel()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	exp := time.Now().Add(time.Minute).Unix()
	sig := signShareAsset(secret, "link-1", "att-1", exp)
	future := strconv.FormatInt(exp+3600, 10)
	if verifyShareAsset(secret, "link-1", "att-1", future, sig, time.Now()) {
		t.Fatal("a signature validated against a tampered (extended) exp")
	}
}

func TestShareAssetSig_UnconfiguredSecret(t *testing.T) {
	// A too-short secret (self-host without an encryption key) cannot sign —
	// signShareAsset returns "" so the mint side degrades to the placeholder,
	// and verify never accepts.
	t.Parallel()
	short := []byte("tooshort")
	exp := time.Now().Add(time.Minute).Unix()
	if sig := signShareAsset(short, "link-1", "att-1", exp); sig != "" {
		t.Fatalf("signShareAsset with a <16B secret returned %q, want empty", sig)
	}
	if verifyShareAsset(short, "link-1", "att-1", strconv.FormatInt(exp, 10), "deadbeef", time.Now()) {
		t.Fatal("verifyShareAsset accepted with a <16B secret")
	}
}

// --- HTTP fixture -------------------------------------------------------

// shareAssetFixture is a workspace with one collection + one item, plus a
// second item, wired for the anchoring matrix. The server has attachments
// and a claim secret so protected-link signing works.
type shareAssetFixture struct {
	srv     *Server
	wsID    string
	slug    string
	collID  string
	itemID  string
	item2ID string
	ownerID string
}

func newShareAssetFixture(t *testing.T) shareAssetFixture {
	t.Helper()
	srv := testServer(t)
	dir := t.TempDir()
	fs, err := attachments.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)
	srv.SetClaimSecret(bytes.Repeat([]byte{0x5a}, 32))

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "ShareAsset"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Shared", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	item2, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Other", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem2: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return shareAssetFixture{
		srv: srv, wsID: ws.ID, slug: ws.Slug, collID: col.ID,
		itemID: item.ID, item2ID: item2.ID, ownerID: owner.ID,
	}
}

// seedImage creates an image original anchored to itemID plus its thumb-md
// variant, each with distinct bytes, and returns (originalID, thumbMdBytes).
func (f shareAssetFixture) seedImage(t *testing.T, itemID string, seed byte) (string, []byte) {
	t.Helper()
	origBody := distinctPNG(t, seed)
	mdBody := distinctPNG(t, seed+1)
	orig := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &itemID, Filename: "orig.png",
	}, origBody)
	variant := models.AttachmentVariantThumbMd
	putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &itemID, ParentID: &orig.ID,
		Variant: &variant, Filename: "orig-thumb-md.png",
	}, mdBody)
	return orig.ID, mdBody
}

// setContent overwrites an item's markdown body.
func (f shareAssetFixture) setContent(t *testing.T, itemID, content string) {
	t.Helper()
	if _, err := f.srv.store.UpdateItem(itemID, models.ItemUpdate{Content: &content}); err != nil {
		t.Fatalf("UpdateItem content: %v", err)
	}
}

func imageRef(id string) string { return fmt.Sprintf("![pic](pad-attachment:%s)", id) }

// createLink mints a share link for the fixture's item or collection.
func (f shareAssetFixture) createLink(t *testing.T, targetType, targetID string, opts *store.ShareLinkOptions) *models.ShareLink {
	t.Helper()
	link, err := f.srv.store.CreateShareLink(f.wsID, targetType, targetID, "view", f.ownerID, opts)
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}
	return link
}

// resolvedRefs resolves a share link (optionally with a password header) and
// returns its attachment_refs map.
func (f shareAssetFixture) resolvedRefs(t *testing.T, token, password string) map[string]shareAttachmentRef {
	t.Helper()
	headers := map[string]string{}
	if password != "" {
		headers["X-Share-Password"] = password
	}
	rr := doRequestWithHeaders(f.srv, "GET", "/api/v1/s/"+token, nil, headers)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve share link: status %d, body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Refs map[string]shareAttachmentRef `json:"attachment_refs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse resolve payload: %v", err)
	}
	return resp.Refs
}

// getAsset issues GET on the asset endpoint. rawQuery is appended verbatim
// (already URL-formed, no leading '?').
func (f shareAssetFixture) getAsset(token, attID, rawQuery string) *httptest.ResponseRecorder {
	path := "/api/v1/s/" + token + "/attachments/" + attID
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	return doRequest(f.srv, "GET", path, nil)
}

// --- fork-1: plain links, max_views rides the page view -----------------

func TestShareAsset_PlainMultiImageUnderMaxViews1(t *testing.T) {
	// fork-1 pin (a): a page under max_views=1 renders ALL its images. The
	// single page open burns the one view; the asset fetches must NOT
	// re-check or decrement it, or the 2nd (and every later) image 404s.
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, md1 := f.seedImage(t, f.itemID, 0x11)
	a2, md2 := f.seedImage(t, f.itemID, 0x21)
	f.setContent(t, f.itemID, imageRef(a1)+"\n\n"+imageRef(a2))

	one := 1
	link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{MaxViews: &one})

	// One page open — burns the single view.
	refs := f.resolvedRefs(t, link.Token, "")
	if _, ok := refs[a1]; !ok {
		t.Fatalf("ref for a1 missing; refs=%v", refs)
	}
	if _, ok := refs[a2]; !ok {
		t.Fatalf("ref for a2 missing; refs=%v", refs)
	}

	// Both assets must serve, AFTER the view budget is spent.
	for _, tc := range []struct {
		id   string
		want []byte
	}{{a1, md1}, {a2, md2}} {
		rr := f.getAsset(link.Token, tc.id, "variant=thumb-md")
		if rr.Code != http.StatusOK {
			t.Fatalf("asset %s: status %d (assets must not consume max_views), body %s", tc.id, rr.Code, rr.Body.String())
		}
		if !bytes.Equal(rr.Body.Bytes(), tc.want) {
			t.Errorf("asset %s: served wrong bytes (want the thumb-md variant, got %d bytes)", tc.id, rr.Body.Len())
		}
	}
}

func TestShareAsset_PlainServesThumbMdByDefault(t *testing.T) {
	// fork-2 pin #5 baseline: a plain link's asset loads with no signature,
	// and defaults to the thumb-md variant when no variant param is given.
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, md1 := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, nil)

	refs := f.resolvedRefs(t, link.Token, "")
	if ref, ok := refs[a1]; !ok || ref.Sig != "" {
		t.Fatalf("plain link ref should exist with empty sig; got %+v ok=%v", ref, ok)
	}
	rr := f.getAsset(link.Token, a1, "") // no variant param → thumb-md default
	if rr.Code != http.StatusOK {
		t.Fatalf("plain asset (no query): status %d, body %s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), md1) {
		t.Errorf("default served the wrong variant: got %d bytes, want thumb-md", rr.Body.Len())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store", cc)
	}
}

func TestShareAsset_ExpiredAndRevoked404(t *testing.T) {
	// fork-1 pin (b): an expired OR revoked token 404s on the asset path even
	// with a valid attachment id. Revocation is explicit (delete), not merely
	// expiry.
	t.Parallel()

	t.Run("expired", func(t *testing.T) {
		f := newShareAssetFixture(t)
		a1, _ := f.seedImage(t, f.itemID, 0x11)
		f.setContent(t, f.itemID, imageRef(a1))
		past := time.Now().Add(-time.Hour).Format(time.RFC3339)
		link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{ExpiresAt: &past})
		rr := f.getAsset(link.Token, a1, "variant=thumb-md")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expired token asset: status %d, want 404", rr.Code)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		f := newShareAssetFixture(t)
		a1, _ := f.seedImage(t, f.itemID, 0x11)
		f.setContent(t, f.itemID, imageRef(a1))
		link := f.createLink(t, "item", f.itemID, nil)
		// Valid before revocation.
		if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusOK {
			t.Fatalf("pre-revoke asset: status %d, want 200", rr.Code)
		}
		if err := f.srv.store.DeleteShareLink(link.ID, f.wsID); err != nil {
			t.Fatalf("DeleteShareLink: %v", err)
		}
		if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
			t.Fatalf("revoked token asset: status %d, want 404", rr.Code)
		}
	})
}

// --- fork-2: protected links require a signed ref -----------------------

func TestShareAsset_ProtectedRequiresSignature(t *testing.T) {
	// fork-2 pin #1: a protected-link asset URL with the correct token + id
	// but NO signature 404s. And pin #2: the signed ref the resolve response
	// mints loads via a plain GET (what an <img src> issues).
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, md1 := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{Password: "hunter2"})

	// Unsigned (bare token+id) → 404. This is the hole option B would reopen.
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("protected asset without signature: status %d, want 404", rr.Code)
	}

	// The minted ref (obtained only AFTER satisfying the password) carries a
	// signature; it loads via a plain GET.
	refs := f.resolvedRefs(t, link.Token, "hunter2")
	ref, ok := refs[a1]
	if !ok || ref.Sig == "" {
		t.Fatalf("protected link should mint a signed ref; got %+v ok=%v", ref, ok)
	}
	rr := f.getAsset(link.Token, a1, ref.Sig+"&variant=thumb-md")
	if rr.Code != http.StatusOK {
		t.Fatalf("signed protected asset: status %d, body %s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), md1) {
		t.Errorf("signed protected asset served wrong bytes: got %d, want thumb-md", rr.Body.Len())
	}
}

func TestShareAsset_ProtectedRequireAuthAlsoGated(t *testing.T) {
	// require_auth (not just password) is a protected link too: its asset path
	// demands a signature. Without one → 404.
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{RequireAuth: true})
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("require_auth asset without signature: status %d, want 404", rr.Code)
	}
}

func TestShareAsset_ExpiredSignature404(t *testing.T) {
	// fork-2 pin #3 (HTTP level): a signature past its exp 404s on the wire.
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{Password: "hunter2"})

	pastExp := time.Now().Add(-time.Second).Unix()
	sig := signShareAsset(f.srv.claimSecret, link.ID, a1, pastExp)
	q := "exp=" + strconv.FormatInt(pastExp, 10) + "&sig=" + sig + "&variant=thumb-md"
	if rr := f.getAsset(link.Token, a1, q); rr.Code != http.StatusNotFound {
		t.Fatalf("expired-signature asset: status %d, want 404", rr.Code)
	}
}

func TestShareAsset_SignatureForXDoesNotFetchY(t *testing.T) {
	// fork-2 pin #4 (HTTP level): a signature minted for attachment X must not
	// authorize attachment Y — even though both are anchored images under the
	// same protected link.
	t.Parallel()
	f := newShareAssetFixture(t)
	aX, _ := f.seedImage(t, f.itemID, 0x11)
	aY, _ := f.seedImage(t, f.itemID, 0x21)
	f.setContent(t, f.itemID, imageRef(aX)+"\n\n"+imageRef(aY))
	link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{Password: "hunter2"})

	refs := f.resolvedRefs(t, link.Token, "hunter2")
	sigX := refs[aX].Sig
	if sigX == "" {
		t.Fatal("no signature minted for aX")
	}
	// Present X's signature against Y's id.
	if rr := f.getAsset(link.Token, aY, sigX+"&variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("aX's signature fetched aY: status %d, want 404", rr.Code)
	}
	// Sanity: X's signature does fetch X.
	if rr := f.getAsset(link.Token, aX, sigX+"&variant=thumb-md"); rr.Code != http.StatusOK {
		t.Fatalf("aX's signature failed to fetch aX: status %d", rr.Code)
	}
}

// --- anchoring & variants-only ------------------------------------------

func TestShareAsset_ItemShareRejectsForeignItemAttachment(t *testing.T) {
	// An item share serves only its own item's attachments. An attachment
	// anchored to a DIFFERENT item (even in the same workspace) 404s — this is
	// what stops an item share from leaking a sibling item's bytes.
	t.Parallel()
	f := newShareAssetFixture(t)
	foreign, _ := f.seedImage(t, f.item2ID, 0x31) // anchored to item2
	link := f.createLink(t, "item", f.itemID, nil)
	if rr := f.getAsset(link.Token, foreign, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("item share served a foreign item's attachment: status %d, want 404", rr.Code)
	}
}

func TestShareAsset_CollectionShareServesItemsInCollection(t *testing.T) {
	// A collection share serves attachments of any item IN the collection —
	// including item2, which an item share of item1 would reject.
	t.Parallel()
	f := newShareAssetFixture(t)
	a2, md2 := f.seedImage(t, f.item2ID, 0x31)
	f.setContent(t, f.item2ID, imageRef(a2))
	link := f.createLink(t, "collection", f.collID, nil)

	refs := f.resolvedRefs(t, link.Token, "")
	if _, ok := refs[a2]; !ok {
		t.Fatalf("collection share did not mint a ref for an in-collection item's attachment; refs=%v", refs)
	}
	rr := f.getAsset(link.Token, a2, "variant=thumb-md")
	if rr.Code != http.StatusOK {
		t.Fatalf("collection share asset: status %d, body %s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), md2) {
		t.Errorf("collection share served wrong bytes")
	}
}

func TestShareAsset_SoftDeletedAttachment404(t *testing.T) {
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, nil)
	if err := f.srv.store.SoftDeleteAttachment(a1); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}
	if rr := f.getAsset(link.Token, a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("soft-deleted attachment: status %d, want 404", rr.Code)
	}
}

func TestShareAsset_OriginalVariantExcluded(t *testing.T) {
	// variant=original is out of scope (originals never ship). It 404s rather
	// than 400 to keep every rejection on this public path non-probing.
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, nil)
	if rr := f.getAsset(link.Token, a1, "variant=original"); rr.Code != http.StatusNotFound {
		t.Fatalf("variant=original: status %d, want 404 (originals out of scope)", rr.Code)
	}
}

func TestShareAsset_NoVariantRow404(t *testing.T) {
	// An image with no variant AND no way to derive one (this fixture wires no
	// image processor) 404s — never an original fallback. Under Option C a
	// variant is normally lazy-derived at resolve (see the derive_test file),
	// but when derivation is unavailable or fails the invariant still holds:
	// the page shows the placeholder, and the byte endpoint never serves the
	// original.
	t.Parallel()
	f := newShareAssetFixture(t)
	// Seed an original only, no variant.
	body := distinctPNG(t, 0x41)
	orig := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &f.itemID, Filename: "novariant.png",
	}, body)
	f.setContent(t, f.itemID, imageRef(orig.ID))
	link := f.createLink(t, "item", f.itemID, nil)

	// Not minted (no variant).
	refs := f.resolvedRefs(t, link.Token, "")
	if _, ok := refs[orig.ID]; ok {
		t.Errorf("minted a ref for an attachment with no thumb-md variant")
	}
	// And the byte endpoint 404s rather than serving the original.
	if rr := f.getAsset(link.Token, orig.ID, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("no-variant asset: status %d, want 404 (no original fallback)", rr.Code)
	}
}

func TestShareAsset_UnknownTokenAndAttachment404(t *testing.T) {
	t.Parallel()
	f := newShareAssetFixture(t)
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, nil)

	if rr := f.getAsset("nope-not-a-token", a1, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown token: status %d, want 404", rr.Code)
	}
	if rr := f.getAsset(link.Token, "00000000-0000-0000-0000-000000000000", "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown attachment: status %d, want 404", rr.Code)
	}
}

// --- mint-side: unconfigured secret degrades protected links ------------

func TestShareAsset_ProtectedWithoutSecretOmitsRef(t *testing.T) {
	// fork-2 fallback boundary: when the deployment secret is unset, a
	// protected link cannot mint a signed ref, so the ref is OMITTED entirely
	// (the page shows the #1135 placeholder) — never an unsigned bare URL.
	t.Parallel()
	f := newShareAssetFixture(t)
	f.srv.SetClaimSecret(nil) // unconfigured
	a1, _ := f.seedImage(t, f.itemID, 0x11)
	f.setContent(t, f.itemID, imageRef(a1))
	link := f.createLink(t, "item", f.itemID, &store.ShareLinkOptions{Password: "hunter2"})

	refs := f.resolvedRefs(t, link.Token, "hunter2")
	if _, ok := refs[a1]; ok {
		t.Errorf("protected link minted a ref with no secret configured; must omit and degrade to placeholder")
	}
}
