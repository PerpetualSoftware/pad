package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const exportSpoolBufSize = 64 << 10

var errExportTooLarge = errors.New("workspace export exceeds metadata limit")

type exportSpoolError struct {
	err error
}

func (e *exportSpoolError) Error() string { return e.err.Error() }
func (e *exportSpoolError) Unwrap() error { return e.err }

type capWriter struct {
	w     io.Writer
	limit int64
	n     int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		c.n += int64(len(p))
		return 0, errExportTooLarge
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) spoolExport(
	ctx context.Context,
	slug string,
	opts store.ExportOptions,
	trailingNewline bool,
) (f *os.File, size int64, atts []models.Attachment, reached int64, err error) {
	limit := s.effectiveExportMetadataMaxBytes()

	cleanup := func() {
		if f != nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			f = nil
		}
	}

	var cw capWriter
	streamErr := s.store.StreamWorkspaceExport(ctx, slug, opts, func(x *models.WorkspaceExport, exportedAtts []models.Attachment, versions store.VersionIter) error {
		tmp, err := os.CreateTemp(s.exportSpoolDir(), "pad-export-*.json")
		if err != nil {
			return &exportSpoolError{err: fmt.Errorf("create export spool file: %w", err)}
		}
		f = tmp
		atts = exportedAtts

		cw = capWriter{w: f, limit: limit}
		bw := bufio.NewWriterSize(&cw, exportSpoolBufSize)

		if err := models.EncodeWorkspaceExport(bw, x, versions); err != nil {
			if errors.Is(err, errExportTooLarge) || errors.Is(err, context.Canceled) {
				return err
			}
			return &exportSpoolError{err: err}
		}
		if trailingNewline {
			if err := bw.WriteByte('\n'); err != nil {
				if errors.Is(err, errExportTooLarge) {
					return err
				}
				return &exportSpoolError{err: err}
			}
		}
		if err := bw.Flush(); err != nil {
			if errors.Is(err, errExportTooLarge) {
				return err
			}
			return &exportSpoolError{err: err}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return &exportSpoolError{err: fmt.Errorf("rewind export spool file: %w", err)}
		}
		size = cw.n
		return nil
	})
	if streamErr != nil {
		cleanup()
		return nil, 0, nil, cw.n, streamErr
	}
	return f, size, atts, cw.n, nil
}

func parseBoolQuery(r *http.Request, name string, def bool) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	switch strings.ToLower(raw) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid %s value %q: expected true, false, 1, 0, yes, or no", name, raw)
	}
}

func parseIncludeVersions(r *http.Request) (bool, error) {
	return parseBoolQuery(r, "include_versions", true)
}

func parseExportCheck(r *http.Request) (bool, error) {
	return parseBoolQuery(r, "check", false)
}

const (
	exportTooLargePartExport   = "export"
	exportTooLargePartManifest = "manifest"
)

func writeExportTooLarge(w http.ResponseWriter, reached, limit int64, includeVersions, suggestNoVersions bool) {
	var msg string
	if suggestNoVersions {
		msg = fmt.Sprintf(
			"The workspace export passed this server's metadata limit of %d bytes (stopped at %d bytes). Export again without version history (pad workspace export --no-versions, or include_versions=false), or ask the operator to raise PAD_EXPORT_METADATA_MAX_BYTES.",
			limit, reached,
		)
	} else {
		msg = fmt.Sprintf(
			"The workspace export passed this server's metadata limit of %d bytes (stopped at %d bytes). Ask the operator to raise PAD_EXPORT_METADATA_MAX_BYTES.",
			limit, reached,
		)
	}
	writeError2(w, http.StatusUnprocessableEntity, "export_too_large", msg, map[string]interface{}{
		"part":             exportTooLargePartExport,
		"size_reached":     reached,
		"limit_bytes":      limit,
		"include_versions": includeVersions,
	})
}

func writeExportManifestTooLarge(w http.ResponseWriter, size, limit int64, includeVersions bool) {
	msg := fmt.Sprintf(
		"The attachment manifest passed this server's metadata limit of %d bytes (%d bytes). Leaving out version history does not shrink it. Export as plain JSON instead (pad workspace export --json), which carries no attachment blobs, or ask the operator to raise PAD_EXPORT_METADATA_MAX_BYTES.",
		limit, size,
	)
	writeError2(w, http.StatusUnprocessableEntity, "export_too_large", msg, map[string]interface{}{
		"part":             exportTooLargePartManifest,
		"size_reached":     size,
		"limit_bytes":      limit,
		"include_versions": includeVersions,
	})
}

func (s *Server) writeSpoolExportError(w http.ResponseWriter, r *http.Request, reached int64, includeVersions bool, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	if errors.Is(err, errExportTooLarge) {
		writeExportTooLarge(w, reached, s.effectiveExportMetadataMaxBytes(), includeVersions, includeVersions)
		return
	}
	if errors.Is(err, store.ErrExportManifest) {
		writeInternalError(w, err)
		return
	}
	var spoolErr *exportSpoolError
	if errors.As(err, &spoolErr) {
		writeInternalError(w, spoolErr.err)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", err.Error())
}
