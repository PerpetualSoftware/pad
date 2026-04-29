//go:build !libvips

package attachments

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	// Stdlib decoders for every format the pure-Go backend supports.
	// image.Decode dispatches via the registered decoders below — we
	// don't call them directly, but the blank imports register them.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/disintegration/imaging"
	// BMP and TIFF live in golang.org/x/image — pulled in via imaging
	// already, but explicit to make the support matrix self-evident.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
)

// pureGoProcessor is the default no-cgo Processor. Constructed via
// NewProcessor — the package exposes a single constructor so callers
// don't need to know which backend they got at compile time. The
// libvips equivalent in `processor_libvips.go` (a future Phase 2 file)
// will provide the same NewProcessor signature behind the libvips
// build tag.
type pureGoProcessor struct {
	caps Capabilities
}

// NewProcessor returns the Processor compiled into this binary. Pure-Go
// build → pureGoProcessor. libvips build → vipsProcessor (Phase 2).
//
// Constructed once at server startup; safe for concurrent use across
// every upload handler (the underlying imaging package operates on
// per-call image.Image values with no shared state).
func NewProcessor() Processor {
	return &pureGoProcessor{
		caps: Capabilities{
			ImageFormats: []string{"png", "jpeg", "gif", "bmp", "tiff"},
			CanTranscode: true,
			MaxPixels:    MaxPixelsDefault,
		},
	}
}

func (p *pureGoProcessor) Capabilities() Capabilities { return p.caps }

// Decode peeks at the image header to enforce MaxPixels BEFORE allocating
// the full pixel buffer. image.DecodeConfig reads only the header bytes
// (a few hundred at most), so an attacker who uploads a "claimed
// 100k x 100k pixels" PNG can't OOM us — we reject before image.Decode
// allocates the row buffers.
//
// The header peek requires we read all of `r` once into a buffer so we
// can replay it for the actual Decode. This costs a single io.ReadAll
// — bounded by the upload-handler's MaxBytesReader (25 MiB by default).
func (p *pureGoProcessor) Decode(r io.Reader) (image.Image, string, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("attachments: read image bytes: %w", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		// image.Decode would also fail; bail with a clearer error
		// message that distinguishes "format unknown" from "format
		// known, decoding failed mid-way" (a corrupt-bytes case).
		return nil, "", fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	if !p.formatSupported(format) {
		return nil, format, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, format, fmt.Errorf("%w: zero dimension (%dx%d)",
			ErrUnsupportedFormat, cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(p.caps.MaxPixels) {
		return nil, format, fmt.Errorf("%w: %dx%d exceeds %d",
			ErrImageTooLarge, cfg.Width, cfg.Height, p.caps.MaxPixels)
	}
	img, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		return nil, format, fmt.Errorf("attachments: decode %s: %w", format, err)
	}
	return img, format, nil
}

// Resize fits the longer edge to maxLong, preserving aspect ratio. The
// imaging package picks the right scale factor for whichever edge is
// longer — passing 0 for the other dimension tells it "auto".
func (p *pureGoProcessor) Resize(img image.Image, maxLong int) (image.Image, error) {
	if maxLong <= 0 {
		return nil, fmt.Errorf("attachments: Resize: maxLong must be positive")
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= maxLong && h <= maxLong {
		// Pass-through avoids the encode/decode cost when the input is
		// already smaller than the target — common for thumb-md (1024px)
		// against typical screenshots.
		return img, nil
	}
	if w >= h {
		return imaging.Resize(img, maxLong, 0, imaging.Lanczos), nil
	}
	return imaging.Resize(img, 0, maxLong, imaging.Lanczos), nil
}

// Rotate accepts only multiples of 90 degrees (clockwise). imaging's
// Rotate90/180/270 are exact pixel reorderings — no resampling, no
// quality loss, ideal for the editor's rotation tool (TASK-879).
func (p *pureGoProcessor) Rotate(img image.Image, deg int) (image.Image, error) {
	// Normalize to [0, 360) so callers can pass -90, 270, etc. and get
	// the same rotation. Using positive modulo (Go's % is sign-preserving)
	// so -90 → 270 instead of -90.
	d := ((deg % 360) + 360) % 360
	switch d {
	case 0:
		return img, nil
	case 90:
		return imaging.Rotate90(img), nil
	case 180:
		return imaging.Rotate180(img), nil
	case 270:
		return imaging.Rotate270(img), nil
	default:
		return nil, fmt.Errorf("attachments: Rotate: only multiples of 90 supported, got %d", deg)
	}
}

// Crop intersects rect with the image bounds and returns the sub-image.
// An empty intersection is rejected — encoding a 0x0 image produces
// invalid output that the next decode would fail on.
func (p *pureGoProcessor) Crop(img image.Image, rect image.Rectangle) (image.Image, error) {
	clipped := rect.Intersect(img.Bounds())
	if clipped.Empty() {
		return nil, fmt.Errorf("attachments: Crop: rect %v is outside image bounds %v",
			rect, img.Bounds())
	}
	return imaging.Crop(img, clipped), nil
}

// Encode writes img to w in the requested format. Only "png" and "jpeg"
// are supported — these are the two formats the thumbnail pipeline
// emits, and the only ones every browser renders without question.
// Other formats fall through to ErrUnsupportedFormat so callers can
// see the limitation explicitly.
func (p *pureGoProcessor) Encode(img image.Image, format string, w io.Writer) error {
	switch format {
	case "png":
		return png.Encode(w, img)
	case "jpeg", "jpg":
		// Quality 85 is the standard sweet spot — visibly identical
		// to 100% on screen, ~3x smaller. Higher would balloon
		// thumbnail storage; lower introduces visible blocking.
		return jpeg.Encode(w, img, &jpeg.Options{Quality: 85})
	default:
		return fmt.Errorf("%w: encode target %q", ErrUnsupportedFormat, format)
	}
}

func (p *pureGoProcessor) formatSupported(format string) bool {
	for _, f := range p.caps.ImageFormats {
		if f == format {
			return true
		}
	}
	return false
}

// ThumbnailFormat picks the best output format for a thumbnail
// derived from an input of the given format. PNG inputs stay PNG so
// transparency survives; everything else encodes as JPEG (smaller
// files, good enough for thumbnails). Exported so the upload pipeline
// and tests share a single source of truth.
func ThumbnailFormat(inputFormat string) string {
	if inputFormat == "png" {
		return "png"
	}
	return "jpeg"
}

// ThumbnailMime returns the canonical MIME type for a thumbnail
// encoded in `format` (paired with ThumbnailFormat above).
func ThumbnailMime(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "jpeg", "jpg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
}

// ThumbnailExt returns the file extension for a thumbnail encoded in
// `format`. Used to build a synthetic filename for the derived row
// (parent's basename + variant + extension) so downloads with
// Content-Disposition expose a sensible filename.
func ThumbnailExt(format string) string {
	switch format {
	case "png":
		return ".png"
	case "jpeg", "jpg":
		return ".jpg"
	default:
		return ".bin"
	}
}
