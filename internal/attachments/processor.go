package attachments

import (
	"errors"
	"image"
	"io"
)

// Processor abstracts image decode / transform / encode operations so
// that two implementations can coexist:
//
//   - Pure-Go (default, no build tag) — `processor_purego.go`. Uses
//     `github.com/disintegration/imaging` and the stdlib decoders. Keeps
//     Pad's single-binary distribution intact (no cgo). Handles PNG /
//     JPEG / GIF / BMP / TIFF for all ops; WebP / AVIF / HEIC are
//     accepted on upload but rejected at the processor level — the
//     editor surface uses Capabilities() to gate rotate/crop UI for
//     those formats.
//
//   - libvips (`-tags libvips`, requires cgo) — Phase 2 / Pad Cloud
//     Docker. Faster, lower-memory, and adds native WebP / AVIF / HEIC
//     processing. The interface is intentionally simple so the libvips
//     backend can wrap a *vips.Image internally and only round-trip
//     through image.Image at the API boundary.
//
// Methods are safe for concurrent use; backends may share state (e.g.
// libvips' global concurrency pool) but must not introduce per-call
// races. All methods MUST be self-contained — never assume call order.
type Processor interface {
	// Decode reads bytes from r and returns the decoded image plus the
	// detected format ("png", "jpeg", "gif", "bmp", "tiff", …). The
	// returned format is the canonical lowercase name; callers can pass
	// it back to Encode unchanged. If r holds bytes that this backend
	// can't decode, the error wraps ErrUnsupportedFormat.
	Decode(r io.Reader) (img image.Image, format string, err error)

	// Resize returns a new image with the longer edge fitted to maxLong,
	// preserving aspect ratio. Images already within maxLong are
	// returned unchanged so callers don't pay the encode cost on
	// pass-through. maxLong must be positive.
	Resize(img image.Image, maxLong int) (image.Image, error)

	// Rotate returns a new image rotated by deg degrees clockwise.
	// Only multiples of 90 (90, 180, 270, plus their negatives) are
	// supported in Phase 1 — that's what the editor's rotation tool
	// (TASK-879) emits, and it sidesteps the resampling cost of
	// arbitrary-angle rotation.
	Rotate(img image.Image, deg int) (image.Image, error)

	// Crop returns the rectangular sub-image bounded by `rect`. The
	// rectangle is interpreted in the image's coordinate space (origin
	// top-left) and intersected with the image bounds — a rect that
	// extends past the bounds is silently clipped rather than rejected,
	// so the editor's crop tool (TASK-880) can be lazy with rounding.
	Crop(img image.Image, rect image.Rectangle) (image.Image, error)

	// Encode writes img to w in the given format ("png" / "jpeg").
	// JPEG is encoded at quality 85 — small enough for thumbnails,
	// high enough that pixel-level lossy artifacts are rare on the
	// kinds of content (screenshots, photos, diagrams) Pad sees most.
	// Unknown formats wrap ErrUnsupportedFormat.
	Encode(img image.Image, format string, w io.Writer) error

	// Capabilities reports what this backend can do. The editor reads
	// these flags via GET /api/v1/server/capabilities and gates
	// rotate/crop UI on per-format support, with an explanatory tooltip
	// when disabled. Uploads always succeed (the MIME allowlist is the
	// gate); display always works (browsers handle WebP / AVIF / HEIC
	// natively). Self-hosters with the pure-Go build see their image
	// formats list shrink, never an upload rejection.
	Capabilities() Capabilities
}

// Capabilities describes the static capability profile of a Processor.
// It's safe to embed in HTTP responses — nothing here changes between
// requests, so callers can cache for the lifetime of the binary.
type Capabilities struct {
	// ImageFormats are the canonical lowercase format names the backend
	// can decode AND encode (i.e. fully supports for transformation).
	// Pure-Go: ["png", "jpeg", "gif", "bmp", "tiff"]; libvips adds
	// "webp", "avif", "heic" on top.
	ImageFormats []string `json:"image_formats"`

	// CanTranscode reports whether the backend can re-encode between
	// formats. Pure-Go is true (it can decode any supported format and
	// encode to PNG / JPEG); libvips is also true. Effectively a
	// future-proofing flag — false would indicate a degraded build.
	CanTranscode bool `json:"can_transcode"`

	// MaxPixels is the hard ceiling on input image area (width * height).
	// Decode rejects bigger images before allocating decoded pixel
	// buffers — the rejection is on raw dimensions read from the file
	// header, not on the decoded buffer. 64 megapixels (8000 * 8000)
	// is generous enough for high-end DSLRs and low enough that the
	// decode buffer (~256 MiB at 4 bytes/pixel) doesn't OOM the server
	// under concurrent uploads.
	MaxPixels int `json:"max_pixels"`
}

// MaxPixelsDefault is the default value used by the pure-Go backend.
// Public so the Capabilities struct's MaxPixels field has a single
// source of truth and so tests can reference it.
const MaxPixelsDefault = 8000 * 8000

// ErrUnsupportedFormat is wrapped by Decode when the input bytes are
// in a format the backend cannot decode (e.g. pure-Go on WebP), and by
// Encode when the requested output format is not "png" or "jpeg".
//
// Callers handle this by skipping the operation, not by failing the
// surrounding flow — the upload itself succeeded, the user sees the
// original at native resolution, and the editor disables the rotate /
// crop UI for that format.
var ErrUnsupportedFormat = errors.New("attachments: image format not supported by this processor")

// ErrImageTooLarge is wrapped by Decode when the input image's
// pixel area exceeds MaxPixels. Reported separately from
// ErrUnsupportedFormat because the format IS supported — only the
// size isn't — and the upload-side caller may want to surface a
// distinct user-facing message.
var ErrImageTooLarge = errors.New("attachments: image dimensions exceed processor limit")
