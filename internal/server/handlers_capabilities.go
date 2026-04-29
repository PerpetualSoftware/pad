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
}

// handleServerCapabilities reports what this build can do to the editor.
// Accessible without auth — the editor needs to know whether to gate
// rotate/crop UI before the user even logs in (e.g. on the share page,
// where the same editor preview path may run). Returns a degraded
// "no processor configured" body when SetImageProcessor was not called,
// rather than 500-ing — that signals to the editor "uploads still work,
// but disable transformation tools."
func (s *Server) handleServerCapabilities(w http.ResponseWriter, r *http.Request) {
	resp := serverCapabilities{}
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
