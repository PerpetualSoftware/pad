//go:build libvips

package attachments

import "log/slog"

// NewProcessor on the libvips build is a placeholder until Phase 2
// ships the real govips-v2-backed implementation (DOC-865,
// "ImageProcessor interface with build-tagged backends" decision).
//
// We return `nil` rather than panicking: every call site already
// nil-checks the processor (the upload handler skips thumbnail
// derivation, the capabilities endpoint reports a degraded empty
// formats list, the editor disables rotate/crop UI). That gives a
// libvips-tagged binary the same runtime profile as a self-host
// build that opted out of image processing entirely — uploads
// succeed, originals display, only derived transformations are
// unavailable. Phase 2 swaps this for the real implementation.
//
// The slog.Warn at construction time is the loud-but-non-fatal
// signal: operators booting the libvips binary today see a clear
// "this build doesn't have image processing yet" line in their
// logs without losing service availability for everything else.
func NewProcessor() Processor {
	slog.Warn("attachments: libvips backend not implemented yet — image processing disabled. " +
		"Build without the libvips tag for the pure-Go default backend (TASK-878). " +
		"Phase 2 will land the real govips-v2 implementation behind this same tag.")
	return nil
}
