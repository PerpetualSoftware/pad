//go:build libvips

package server

// wireTestImageProcessor (libvips build): no-op. The libvips backend
// in `internal/attachments/processor_libvips.go` is a panicking stub
// until Phase 2 lands the real govips-v2 implementation; calling
// SetImageProcessor with that stub would crash every server test
// even when the test under examination has nothing to do with image
// processing. Skipping the wire here keeps the rest of the server
// test surface compilable + runnable under `-tags libvips`. Tests
// that DO require a working processor are tagged `!libvips` so they
// only run on the default build until Phase 2 ships.
func wireTestImageProcessor(_ *Server) {}
