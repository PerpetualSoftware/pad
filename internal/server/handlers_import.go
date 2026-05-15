package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/urlimport"
)

// importURLRequest is the POST /api/v1/import/url request body.
type importURLRequest struct {
	URL string `json:"url"`
}

// importURLResponse is the response shape from POST /api/v1/import/url.
// Side-effect-free: no database writes happen during the call; the
// client decides whether to splice the markdown into an item.
type importURLResponse struct {
	Markdown     string `json:"markdown"`
	DetectedType string `json:"detected_type"` // "openapi" or "generic"
	Title        string `json:"title,omitempty"`
	SourceURL    string `json:"source_url"`   // final URL after redirects
	FetchedAt    string `json:"fetched_at"`   // RFC3339 timestamp
	ContentType  string `json:"content_type"` // upstream Content-Type
}

// importURLFetcher is the package-level Fetcher reused across requests.
// It owns a memoized safe transport per the urlimport package; building
// per-request would leak idle-connection pools. Initialized lazily on
// the first request so tests that swap pkgFetcher in TestMain can do so
// before any request arrives.
var pkgFetcher = urlimport.NewFetcher()

// importURLTimeout caps total handler time including conversion.
// Fetcher's own timeout is the HTTP-level cap; this is the wall-clock
// budget for the whole pipeline so a pathologically slow converter
// still releases the connection.
const importURLTimeout = 30 * time.Second

// handleImportURL implements POST /api/v1/import/url.
//
//   - Body:   {"url": "https://..."}
//   - Result: {"markdown", "detected_type", "title", "source_url",
//     "fetched_at", "content_type"}
//
// Pipeline:
//  1. Validate URL (scheme/credentials/IP-literal SSRF pre-flight).
//  2. Fetch with size + time caps and dial-time SSRF re-validation.
//  3. Detect document type (openapi vs generic).
//  4. Convert:
//     - openapi → ConvertOpenAPI. Falls back to ConvertGeneric on
//     v2 specs or other "openapi-detected but not 3.x" cases so a
//     misdetected JSON spec still produces usable output.
//     - generic → ConvertGeneric.
//
// Errors:
//   - 400 for malformed URL, SSRF rejection, size cap exceeded
//   - 502 for upstream failures (non-2xx, DNS, timeout)
//   - 422 when the fetched body cannot be converted (rare)
//
// The handler never writes to the database. Callers (the editor's
// "Insert from URL" modal) are responsible for any item updates.
func (s *Server) handleImportURL(w http.ResponseWriter, r *http.Request) {
	// RequireAuth middleware already gates the /api/v1 group when users
	// exist; in the no-user "fresh install" mode the middleware lets
	// requests through with currentUserID == "". We use the ID only for
	// log attribution, never for authorization decisions.
	userID := currentUserID(r)

	var input importURLRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	if input.URL == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url is required")
		return
	}

	// Pre-flight URL validation up front — gives a precise 400 instead
	// of a generic upstream error. The Fetcher also runs ValidateURL
	// but this lets us distinguish "you typed garbage" from "we tried
	// to fetch and the server rejected us".
	//
	// When the fetcher is configured with AllowLocal (test mode), skip
	// the pre-flight so httptest's 127.0.0.1 servers stay reachable.
	// The fetcher itself respects AllowLocal at both validate and dial
	// time, so the security guard is intact for production callers.
	if !pkgFetcher.AllowLocal {
		if err := urlimport.ValidateURL(input.URL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), importURLTimeout)
	defer cancel()

	fetched, err := pkgFetcher.Fetch(ctx, input.URL)
	if err != nil {
		// 502 mirrors the gateway-error pattern used elsewhere for
		// upstream failures. SSRF rejection from the dial-time guard
		// is mapped to 400 because the URL is the user's input.
		status := http.StatusBadGateway
		code := "fetch_failed"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			code = "fetch_timeout"
		}
		slog.Info("import url fetch failed", "user", userID, "url", input.URL, "error", err)
		writeError(w, status, code, err.Error())
		return
	}

	detected := urlimport.Detect(fetched.ContentType, fetched.Body)

	var result *urlimport.ConvertResult
	switch detected {
	case urlimport.TypeOpenAPI:
		result, err = urlimport.ConvertOpenAPI(fetched.Body, fetched.URL)
		if err != nil {
			// Detected as openapi but conversion failed — most common
			// reason is a Swagger 2.0 spec sniffed as "openapi" but
			// rejected by ConvertOpenAPI. Fall back to generic so the
			// user at least gets the raw textual content. Re-classify
			// the response accordingly so the UI can show the right
			// affordance.
			slog.Info("openapi conversion fell back to generic", "user", userID, "url", input.URL, "error", err)
			result, err = urlimport.ConvertGeneric(fetched.Body, fetched.URL)
			detected = urlimport.TypeGeneric
		}
	default:
		result, err = urlimport.ConvertGeneric(fetched.Body, fetched.URL)
	}
	if err != nil {
		slog.Info("import url conversion failed", "user", userID, "url", input.URL, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "conversion_failed", err.Error())
		return
	}

	resp := importURLResponse{
		Markdown:     result.Markdown,
		DetectedType: string(detected),
		Title:        result.Title,
		SourceURL:    fetched.URL,
		FetchedAt:    time.Now().UTC().Format(time.RFC3339),
		ContentType:  fetched.ContentType,
	}
	writeJSON(w, http.StatusOK, resp)
}
