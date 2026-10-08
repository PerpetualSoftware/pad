package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// BUG-3476: `pad workspace import` of a bundle printed nothing until the
// response, could sit silent for minutes on a dead TCP path (a blocked body
// write fails only at the kernel's retransmit limit), and when the answer was
// lost it could not say whether the workspace exists. These are the pieces the
// command watches the upload with: a reader that counts what the transport has
// taken, a context-aware upload, and the outcome lookup BUG-3475 added.

// CountingReader counts the bytes read through it and when the last read
// happened. The HTTP transport reads the body only as the connection accepts
// it, so a read that stops arriving is a stalled upload.
type CountingReader struct {
	r        io.Reader
	n        atomic.Int64
	lastRead atomic.Int64 // UnixNano of the latest read that returned bytes
}

// NewCountingReader wraps r. The stall clock starts now, so a connection that
// never starts reading counts as stalled too.
func NewCountingReader(r io.Reader) *CountingReader {
	c := &CountingReader{r: r}
	c.lastRead.Store(time.Now().UnixNano())
	return c
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
		c.lastRead.Store(time.Now().UnixNano())
	}
	return n, err
}

// Sent is how many bytes the transport has taken.
func (c *CountingReader) Sent() int64 { return c.n.Load() }

// SinceLastRead is how long ago the transport last took bytes.
func (c *CountingReader) SinceLastRead() time.Duration {
	return time.Since(time.Unix(0, c.lastRead.Load()))
}

// PostStreamContext is PostStreamWithContentTypeHeaders under a context, so a
// caller watching the upload can abandon it.
func (c *Client) PostStreamContext(ctx context.Context, path string, body io.Reader, contentType string, result interface{}) (http.Header, error) {
	req, err := c.newRequest("POST", path, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	header := resp.Header
	return header, c.handleResponse(resp, result)
}

// Import outcome states, as GET /workspaces/import-status reports them
// (BUG-3475, internal/server/import_outcomes.go).
const (
	ImportStateRunning    = "running"
	ImportStateComplete   = "complete"
	ImportStateKept       = "kept"
	ImportStateRemoved    = "removed"
	ImportStateNotCreated = "not_created"
	ImportStateUnknown    = "unknown"
)

// ImportStatus is what the server knows about one import attempt.
type ImportStatus struct {
	State         string `json:"state"`
	WorkspaceSlug string `json:"workspace_slug,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	OwnerUsername string `json:"owner_username,omitempty"`
}

// ErrImportStatusUnknown is the server holding no record of the key: never
// started there, expired, lost to a restart, or a server older than BUG-3475,
// which ignores the key. It is an UNKNOWN outcome, never "nothing created".
var ErrImportStatusUnknown = errors.New("the server holds no record of this import")

// GetImportStatus asks what became of the caller's import attempt with key.
func (c *Client) GetImportStatus(key string) (*ImportStatus, error) {
	var st ImportStatus
	if err := c.get("/workspaces/import-status?key="+url.QueryEscape(key), &st); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "not_found" {
			return nil, ErrImportStatusUnknown
		}
		return nil, err
	}
	return &st, nil
}
