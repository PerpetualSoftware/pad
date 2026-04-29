//go:build !libvips

package server

import "github.com/PerpetualSoftware/pad/internal/attachments"

// wireTestImageProcessor (pure-Go build): wires the real pure-Go
// processor so thumbnail integration tests can derive variants
// against a working backend. The libvips counterpart in
// `testimageprocessor_libvips_test.go` is a no-op until Phase 2
// ships a libvips test backend.
func wireTestImageProcessor(srv *Server) {
	srv.SetImageProcessor(attachments.NewProcessor())
}
