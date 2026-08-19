package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/attachments"
)

// serverCapabilities is the response shape for GET /api/v1/server/capabilities.
// Currently surfaces only the image processor's static capability profile —
// future capability flags (FTS dialect, Stripe configured, etc.) can extend
// this struct without breaking the existing image fields.
//
// Static for the lifetime of the binary, so clients are free to cache.
type serverCapabilities struct {
	Image attachments.Capabilities `json:"image"`

	// CollectionResolution is true when this build resolves a collection slug
	// server-side with exact-match-first + the singular/alias fallback and the
	// archived-claims refusal (resolveItemCollectionSlug, BUG-2578/2630). The
	// CLI reads it to decide, on a collection-not-found, whether that answer is
	// AUTHORITATIVE (this build already tried every alias, so the slug is truly
	// absent/archived/hidden — do not retry an alias and defeat the protection)
	// or whether it is talking to an older build with no resolver and should
	// fall back to the legacy client-side alias retry. Always true here; its
	// ABSENCE (an old build that 404s this endpoint or omits the field) is the
	// signal to retry.
	CollectionResolution bool `json:"collection_resolution"`
}

// handleServerCapabilities reports what this build can do to the editor.
// Accessible without auth — the editor needs to know whether to gate
// rotate/crop UI before the user even logs in (e.g. on the share page,
// where the same editor preview path may run). Returns a degraded
// "no processor configured" body when SetImageProcessor was not called,
// rather than 500-ing — that signals to the editor "uploads still work,
// but disable transformation tools."
func (s *Server) handleServerCapabilities(w http.ResponseWriter, r *http.Request) {
	resp := serverCapabilities{CollectionResolution: true}
	if s.imageProcessor != nil {
		resp.Image = s.imageProcessor.Capabilities()
	} else {
		// Empty list + can_transcode false signals the editor to hide
		// the rotation / crop affordances. Originals still upload and
		// display — only derived transformations are unavailable.
		resp.Image = attachments.Capabilities{
			ImageFormats: []string{},
			CanTranscode: false,
			MaxPixels:    0,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
