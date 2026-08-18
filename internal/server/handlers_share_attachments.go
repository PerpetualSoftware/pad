package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// Share-link asset serving (BUG-2389 2b / TASK-2637). This is the byte
// path that lets a public share page render the image ATTACHMENTS embedded
// in its shared content — deliberately VARIANTS ONLY (Dave-authorized:
// rendered thumbnails, never originals or file downloads). The companion
// minting side lives in handlers_share_links.go (mintShareAttachmentRefs).
//
// Two access classes, decided by two forks on the TASK-2637 trail:
//
//   - PLAIN links (no password, no require_auth): token + revocation +
//     expiry + item/collection anchoring gate the bytes. max_views is NOT
//     re-checked — a page open is the billable view; the assets it embeds
//     ride that one granted view (fork-1, Option 2).
//
//   - PROTECTED links (password or require_auth): the password/auth is a
//     SECOND FACTOR whose whole job is surviving URL leakage (share tokens
//     travel through mail, logs, history, link previews). A bare token+id
//     asset URL would leak through the identical channels, reopening the
//     hole the second factor closes — so protected-link assets additionally
//     require a short-lived HMAC signature minted into the page-data
//     response only AFTER the factor was satisfied (fork-2, Option C; the
//     "just anchor it" option B was rejected for exactly this reason).
//
// Every rejection is a generic 404 — no enumeration, no workspace-scope
// leak, byte-identical whether the token is unknown, expired, revoked, the
// attachment is foreign/soft-deleted/not-an-image, or a signature is
// missing/expired/for-a-different-attachment.

const (
	// shareAssetSigDomain domain-separates the share-asset HMAC from every
	// other use of the same deployment secret (notably the 6-digit claim
	// codes in claim_codes.go, which sign a different payload shape). A
	// signature minted here can never be mistaken for — or collide with —
	// one minted anywhere else, even though both key off s.claimSecret.
	shareAssetSigDomain = "share-asset-v1"

	// shareAssetSigTTL is how long a minted asset signature stays valid.
	// Minutes-scale: long enough for a page to render and a viewer to dwell
	// on it, short enough that a leaked asset URL is stale almost
	// immediately. The signature only gates PROTECTED links; plain links
	// need no signature at all.
	shareAssetSigTTL = 10 * time.Minute

	// shareAssetVariant is the single variant this endpoint serves by
	// default and the one mintShareAttachmentRefs gates on. Originals are
	// out of scope by authorization; thumb-md is the rendered variant the
	// share page embeds.
	shareAssetVariant = models.AttachmentVariantThumbMd
)

// signShareAsset produces the hex HMAC binding a share link + attachment +
// expiry together. Returns "" when the secret is too short to sign with
// (an unconfigured self-host deployment) — the caller treats that as
// "cannot mint a protected ref" and degrades to the honest placeholder,
// never to an unsigned bare URL.
//
// The variant is deliberately NOT part of the signed payload: the signature
// authorizes access to THIS attachment's rendered variants under THIS link;
// the byte endpoint independently constrains which variant strings it will
// serve (thumb-sm / thumb-md, never original). Signing the variant would
// buy nothing and would force the mint side to pick the variant the client
// will later request.
func signShareAsset(secret []byte, linkID, attachmentID string, exp int64) string {
	if len(secret) < 16 {
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	writeLenPrefixed(mac, []byte(shareAssetSigDomain))
	writeLenPrefixed(mac, []byte(linkID))
	writeLenPrefixed(mac, []byte(attachmentID))
	var expBytes [8]byte
	binary.BigEndian.PutUint64(expBytes[:], uint64(exp))
	mac.Write(expBytes[:])
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyShareAsset returns true iff sig is a currently-valid signature for
// (linkID, attachmentID, exp) — constant-time compared and not yet expired.
// Any malformed / empty input returns false without leaking which check
// failed.
func verifyShareAsset(secret []byte, linkID, attachmentID, expStr, sig string, now time.Time) bool {
	if len(secret) < 16 || sig == "" || expStr == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if now.Unix() > exp {
		return false
	}
	want := signShareAsset(secret, linkID, attachmentID, exp)
	if want == "" {
		return false
	}
	// hex-decoded, constant-time. Compare the decoded bytes rather than the
	// hex strings so a differing hex case can't shortcut the compare.
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	wantBytes, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, wantBytes) == 1
}

// handleGetShareLinkAttachment serves a rendered image variant for an
// attachment embedded in a shared item/collection's content.
// GET /api/v1/s/{token}/attachments/{attachmentID}
func (s *Server) handleGetShareLinkAttachment(w http.ResponseWriter, r *http.Request) {
	// no-store BEFORE any lookup or gate — every denial below is
	// authorization-dependent and an unlabelled 404 is heuristically
	// cacheable by URL (see handleGetAttachment for the full rationale).
	// The share asset path keeps no-store even on success: a share link can
	// be revoked or expire, and these thumbnails are small, so we make
	// revocation bite immediately rather than trade it for a browser cache.
	w.Header().Set("Cache-Control", "private, no-store")

	if s.attachments == nil {
		// Same generic 404 as every other rejection — a public caller must
		// not learn whether storage is configured.
		writeShareAttachmentNotFound(w)
		return
	}

	token := chi.URLParam(r, "token")
	attachmentID := chi.URLParam(r, "attachmentID")
	if token == "" || attachmentID == "" {
		writeShareAttachmentNotFound(w)
		return
	}

	// 1. Resolve the link. Unknown / revoked (deleted → nil) → 404.
	link, err := s.store.GetShareLinkByToken(token)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if link == nil {
		writeShareAttachmentNotFound(w)
		return
	}
	// Expiry / max_views validity. NOTE: ValidateShareLink checks expiry;
	// it does NOT decrement or re-enforce max_views (that lives in
	// RecordShareLinkView, which we deliberately do NOT call here — fork-1
	// Option 2: assets ride the page's granted view).
	if err := s.store.ValidateShareLink(link); err != nil {
		writeShareAttachmentNotFound(w)
		return
	}

	// 2. Protected links (password OR require_auth) require a valid signed
	//    ref. Plain links need none.
	if link.HasPassword || link.RequireAuth {
		q := r.URL.Query()
		if !verifyShareAsset(s.claimSecret, link.ID, attachmentID, q.Get("exp"), q.Get("sig"), time.Now()) {
			writeShareAttachmentNotFound(w)
			return
		}
	}

	// 3. Anchor: the attachment must be a LIVE IMAGE that belongs to the
	//    shared target. Any miss → generic 404 (no enumeration).
	att, err := s.store.GetAttachment(attachmentID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !s.shareAttachmentAnchors(link, att) {
		writeShareAttachmentNotFound(w)
		return
	}

	// 4. Variants only. Resolve the requested (default thumb-md) variant,
	//    workspace-scoped. No original fallback — that is the whole point of
	//    "variants only". Missing variant row → 404.
	variant := shareAssetVariant
	if v := r.URL.Query().Get("variant"); v != "" {
		if !isShareServableVariant(v) {
			// Unknown or original — 404, not 400, to keep every rejection
			// on this public path byte-identical and non-probing.
			writeShareAttachmentNotFound(w)
			return
		}
		variant = v
	}
	derived, err := s.store.GetAttachmentVariant(link.WorkspaceID, att.ID, variant)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if derived == nil {
		writeShareAttachmentNotFound(w)
		return
	}

	// 5. Serve the variant bytes, fail-closed on Content-Type exactly like
	//    the authenticated handler. Variants are images, but we still route
	//    the MIME through the allowlist so a mislabelled row can never render
	//    as active same-origin content.
	s.serveShareAttachmentBytes(w, r, derived)
}

// shareAttachmentAnchors reports whether att is a live image that belongs to
// this share link's target — an item-share serves only its own item's
// attachments; a collection-share serves attachments of any item IN the
// shared collection. Everything else (nil, soft-deleted, orphan, non-image,
// foreign workspace, foreign item/collection) is rejected. Fail-closed.
func (s *Server) shareAttachmentAnchors(link *models.ShareLink, att *models.Attachment) bool {
	if att == nil || att.DeletedAt != nil || att.ItemID == nil {
		return false
	}
	if att.WorkspaceID != link.WorkspaceID {
		return false
	}
	// Image-only, via the allowlist (not a bare "image/" prefix) so only
	// genuinely-servable image MIMEs pass — matches the fail-closed serve.
	if entry, ok := attachments.LookupMIME(att.MimeType); !ok || entry.Category != attachments.CategoryImage {
		return false
	}
	switch link.TargetType {
	case "item":
		return *att.ItemID == link.TargetID
	case "collection":
		item, err := s.store.GetItem(*att.ItemID)
		if err != nil || item == nil {
			return false
		}
		return item.CollectionID == link.TargetID
	default:
		return false
	}
}

// serveShareAttachmentBytes streams a variant's bytes. Mirrors the byte tail
// of handleGetAttachment: fail-closed Content-Type off the allowlist,
// nosniff, ServeContent when the backend seeks. Content-Disposition is
// always inline here (variants are images the share page embeds) but the
// MIME is still validated so an unrecognized row serves as opaque octets.
func (s *Server) serveShareAttachmentBytes(w http.ResponseWriter, r *http.Request, att *models.Attachment) {
	store, err := s.attachments.Resolve(att.StorageKey)
	if err != nil {
		writeInternalError(w, fmt.Errorf("resolve share attachment store for %s: %w", att.StorageKey, err))
		return
	}
	body, err := store.Get(r.Context(), att.StorageKey)
	if err != nil {
		if errors.Is(err, attachments.ErrNotFound) {
			slog.Warn("share attachments: blob missing for live variant row",
				"attachment_id", att.ID, "storage_key", att.StorageKey)
			writeShareAttachmentNotFound(w)
			return
		}
		writeInternalError(w, fmt.Errorf("get share attachment blob: %w", err))
		return
	}
	defer body.Close()

	contentType := att.MimeType
	if entry, ok := attachments.LookupMIME(att.MimeType); !ok || !entry.ServeInline() {
		// Not an inline-safe allowlisted type: don't echo a type the browser
		// might act on. Paired with nosniff + attachment disposition, inert.
		contentType = "application/octet-stream"
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment")
	} else {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "inline")
	}

	if seeker, ok := body.(io.ReadSeeker); ok {
		modtime := att.CreatedAt
		if modtime.IsZero() {
			modtime = time.Now()
		}
		http.ServeContent(w, r, att.Filename, modtime, seeker)
		return
	}
	if size, sErr := store.Stat(r.Context(), att.StorageKey); sErr == nil && size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if r.Method == http.MethodHead {
		return
	}
	if _, copyErr := io.Copy(w, body); copyErr != nil {
		slog.Warn("share attachments: streaming copy failed",
			"attachment_id", att.ID, "error", copyErr)
	}
}

// isShareServableVariant gates the ?variant= query on this public path. It
// EXCLUDES original (unlike isKnownVariant, which includes it) — originals
// are out of the TASK-2637 authorization; only rendered thumbnails ship.
func isShareServableVariant(v string) bool {
	switch v {
	case models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd:
		return true
	}
	return false
}

// writeShareAttachmentNotFound is the single 404 writer for this path, so
// every rejection reason produces a byte-identical response.
func writeShareAttachmentNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "Not found")
}
