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

// WHAT A BUILD THAT CANNOT DECODE A FORMAT ACTUALLY COSTS THE READER
// (BUG-2964, documented here because this endpoint is where a client asks what
// this build can do).
//
// `Image.ImageFormats` is not only about the rotate/crop affordances. Thumbnail
// derivation uses the same processor, so a format absent from that list gets NO
// derived variant on this build — and the pure-Go processor decodes PNG, JPEG,
// GIF, BMP and TIFF, while HEIC and AVIF need the libvips build. Three
// consequences follow, and they differ by surface:
//
//   - IN THE EDITOR, a HEIC attachment is embedded as a downloadable file chip
//     rather than an <img>. The byte endpoint falls back to the original when
//     the variant row is missing, and Chrome and Firefox decode neither HEIC nor
//     HEIF, so an <img> would render the broken-image icon. AVIF stays an image:
//     it has no derived variant here either, but browsers decode it. The client
//     reads `X-Pad-Attachment-Derived` from the byte endpoint to tell these
//     apart per attachment — see web/src/lib/markdown/attachments.ts.
//   - ON PUBLIC SHARE LINKS, a HEIC attachment is unreachable: the share read
//     path serves VARIANTS ONLY and answers 404 when the variant row is missing.
//     That is deliberate and stays. The variant pipeline is the privacy
//     boundary — a stripped, normalized derivative rather than the uploader's
//     original with its EXIF/GPS intact — so serving the original to an
//     anonymous viewer to fix a rendering complaint would trade a broken image
//     for a location leak. The correct refusal, and the wrong outcome for the
//     reader; the fix is to run the libvips build.
//   - PAD CLOUD IS UNAFFECTED. It runs libvips, derives HEIC thumbnails as
//     JPEG, and every surface above gets a decodable variant.
//
// Uploads of these formats are ACCEPTED regardless (BUG-2961): the allowlist is
// not build-dependent, Safari decodes HEIC, and refusing at the door would take
// a working format away from those users to spare others a chip.
//
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
