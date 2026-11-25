package server

import (
	"archive/tar"
	"fmt"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// exportBundleTarCounts holds the exact tar entry and 512-byte header block
// counts that importBundle will accumulate when reading a bundle produced by
// handleExportWorkspaceBundle (TASK-4844).
type exportBundleTarCounts struct {
	Entries      int
	HeaderBlocks int64
}

func (s *Server) effectiveImportBundleMaxEntries() int {
	if s.importBundleMaxEntriesOverride > 0 {
		return s.importBundleMaxEntriesOverride
	}
	return importBundleMaxEntries
}

func (s *Server) effectiveImportBundleMaxHeaderBlocks() int64 {
	if s.importBundleMaxHeaderBlocksOverride > 0 {
		return s.importBundleMaxHeaderBlocksOverride
	}
	return importBundleMaxHeaderBlocks
}

// tarByteCounter counts bytes written by tar.Writer without storing them.
type tarByteCounter int64

func (c *tarByteCounter) Write(p []byte) (int, error) {
	*c += tarByteCounter(len(p))
	return len(p), nil
}

// countExportBundleTar computes the exact tar entry count and 512-byte header
// block count (including PAX/GNU extension headers and the end-of-archive zero
// blocks read by the final tar.Reader.Next) that importBundle will count for
// an export carrying pad-export.json (exportSize), attachments/manifest.json
// (manifestSize), and attachments.
func countExportBundleTar(exportSize, manifestSize int64, attachments []models.Attachment) (exportBundleTarCounts, error) {
	var counter tarByteCounter
	writeOne := func(hdr *tar.Header) error {
		// A fresh tar.Writer per header lets us pass the real hdr.Size (which
		// can affect PAX formatting) without writing any body bytes.
		return tar.NewWriter(&counter).WriteHeader(hdr)
	}

	if err := writeOne(bundleTarHeader("pad-export.json", exportSize, time.Time{})); err != nil {
		return exportBundleTarCounts{}, fmt.Errorf("count pad-export.json header: %w", err)
	}
	if err := writeOne(bundleTarHeader("attachments/manifest.json", manifestSize, time.Time{})); err != nil {
		return exportBundleTarCounts{}, fmt.Errorf("count attachments/manifest.json header: %w", err)
	}
	for i := range attachments {
		if err := writeOne(bundleAttachmentTarHeader(&attachments[i])); err != nil {
			return exportBundleTarCounts{}, fmt.Errorf("count attachment %s header: %w", attachments[i].ID, err)
		}
	}
	// End-of-archive zero blocks written by tar.Writer.Close (2 blocks = 1024
	// bytes). importBundle checks headerBlocks before its io.EOF check, so
	// those blocks count toward the cap too.
	if err := tar.NewWriter(&counter).Close(); err != nil {
		return exportBundleTarCounts{}, fmt.Errorf("count end-of-archive blocks: %w", err)
	}

	return exportBundleTarCounts{
		Entries:      2 + len(attachments),
		HeaderBlocks: int64(counter) / 512,
	}, nil
}

func writeExportBundleEntriesTooLarge(w http.ResponseWriter, counts exportBundleTarCounts, maxEntries int, maxHeaderBlocks int64) {
	var reason string
	switch {
	case counts.Entries > maxEntries && counts.HeaderBlocks > maxHeaderBlocks:
		reason = fmt.Sprintf("%d tar entries, limit %d; %d tar header blocks, limit %d",
			counts.Entries, maxEntries, counts.HeaderBlocks, maxHeaderBlocks)
	case counts.Entries > maxEntries:
		reason = fmt.Sprintf("%d tar entries, limit %d", counts.Entries, maxEntries)
	default:
		reason = fmt.Sprintf("%d tar header blocks, limit %d", counts.HeaderBlocks, maxHeaderBlocks)
	}
	msg := fmt.Sprintf(
		"This workspace has too many attachments for a bundle this server can import (%s). Export as plain JSON instead (pad workspace export --json), which carries no attachment blobs.",
		reason,
	)
	writeError2(w, http.StatusUnprocessableEntity, "export_too_large", msg, map[string]interface{}{
		"entries":             counts.Entries,
		"limit_entries":       maxEntries,
		"header_blocks":       counts.HeaderBlocks,
		"limit_header_blocks": maxHeaderBlocks,
	})
}
