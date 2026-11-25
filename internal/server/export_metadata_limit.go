package server

import "os"

// defaultExportMetadataMaxBytes is the shared size ceiling for workspace
// export metadata (pad-export.json and attachments/manifest.json) on both
// export and import, for tar.gz bundles and plain JSON alike (TASK-4839).
const defaultExportMetadataMaxBytes int64 = 100 << 20 // 100 MiB

// SetExportMetadataMaxBytes overrides the shared metadata cap on workspace
// exports and imports. Values <= 0 fall back to defaultExportMetadataMaxBytes.
// Wired from PAD_EXPORT_METADATA_MAX_BYTES in cmd/pad/cmd_server.go.
func (s *Server) SetExportMetadataMaxBytes(n int64) {
	s.exportMetadataMaxBytes = n
}

func (s *Server) effectiveExportMetadataMaxBytes() int64 {
	if s.exportMetadataMaxBytes > 0 {
		return s.exportMetadataMaxBytes
	}
	return defaultExportMetadataMaxBytes
}

// SetExportSpoolDir overrides the directory used for temporary export spool
// files. An empty string falls back to os.TempDir().
func (s *Server) SetExportSpoolDir(dir string) {
	s.exportSpoolPath = dir
}

func (s *Server) exportSpoolDir() string {
	if s.exportSpoolPath != "" {
		return s.exportSpoolPath
	}
	return os.TempDir()
}
