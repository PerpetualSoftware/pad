package server

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// redactedQueryKeys are query-string keys whose values must never appear
// in request logs. The server's POST endpoints reject these via header-
// only contracts, but a misuse via GET (e.g. an operator pasting
// /setup?token=<x> instead of using the fragment form, or a bug in a
// future feature that accepts the same key) would otherwise persist
// the secret in `logs/server.log` AND in any log-aggregation pipeline.
//
// Add new entries here whenever a new sensitive query key is
// introduced. The list is intentionally short — most secrets should
// move to the request body or a header instead.
var redactedQueryKeys = regexp.MustCompile(`(?i)(\b(?:token|password|secret|api[_-]?key)=)[^&]*`)

// redactQueryString replaces sensitive query-key values with REDACTED
// while leaving the rest of the query string intact for diagnostics.
// Operates on the raw query string to avoid the alphabetization that
// url.Values.Encode() would impose (which would break log diffing
// against the actual request).
func redactQueryString(raw string) string {
	if raw == "" {
		return raw
	}
	return redactedQueryKeys.ReplaceAllString(raw, "${1}REDACTED")
}

// StructuredLogger is a chi-compatible request logger that writes structured
// log entries via slog. It replaces chi's default Logger middleware.
func StructuredLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		duration := time.Since(start)
		status := ww.Status()

		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Duration("duration", duration),
			slog.Int("bytes", ww.BytesWritten()),
		}

		if reqID := chimiddleware.GetReqID(r.Context()); reqID != "" {
			attrs = append(attrs, slog.String("request_id", reqID))
		}

		if r.URL.RawQuery != "" {
			attrs = append(attrs, slog.String("query", redactQueryString(r.URL.RawQuery)))
		}

		slog.LogAttrs(r.Context(), level, "http request", attrs...)
	})
}
