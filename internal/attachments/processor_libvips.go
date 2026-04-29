//go:build libvips

package attachments

// NewProcessor on the libvips build is a placeholder until Phase 2
// ships the real vips-backed implementation (DOC-865, "ImageProcessor
// interface with build-tagged backends" decision). Building with
// `-tags libvips` still compiles cleanly — the panic only fires if
// callers actually instantiate the processor at runtime, which gives
// us a loud, obvious "you booted into a half-finished build" signal
// rather than silently degrading.
//
// The Phase 2 PR will replace this body with the real
// govips-v2-backed implementation (Decode/Resize/Rotate/Crop/Encode
// over a *vips.ImageRef internally). Format-policy helpers
// (ThumbnailFormat / ThumbnailMime / ThumbnailExt) live in the
// untagged processor.go and are shared by both backends as-is.
func NewProcessor() Processor {
	panic("attachments: libvips backend not implemented yet — Phase 2 ships it under -tags libvips. " +
		"Build without the libvips tag for the pure-Go default backend (TASK-878).")
}
