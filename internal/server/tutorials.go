package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// In-app tutorials (TASK-3452, PLAN-3442).
//
// The catalog lives on getpad.dev (pad-web's /learn/catalog.json, built from
// the same file as getpad.dev/learn), so publishing a tutorial needs no Pad
// release. On Pad CLOUD the server fetches it and serves it same-origin: the
// app's CSP keeps connect-src and img-src to itself, so the browser never
// reaches getpad.dev, and the posters are proxied here for the same reason.
// A SELF-HOSTED server never fetches anything: both routes answer 404 and the
// web client links out to getpad.dev/learn instead (a self-hoster's instance
// makes no third-party calls). Lead rulings on TASK-3452: server-side fetch
// gated on cloud mode, the catalog URL a server constant, posters proxied.

// tutorialsCatalogURL is the catalog's address. www, not the apex: the apex
// answers 307 (measured on TASK-3452 U1).
const tutorialsCatalogURL = "https://www.getpad.dev/learn/catalog.json"

// tutorialsPosterOrigin is the only origin a poster is fetched from, whatever
// the catalog says, so a bad catalog cannot point this server at anything else.
const tutorialsPosterOrigin = "https://www.getpad.dev/"

const (
	tutorialsTTL            = time.Hour
	tutorialsRetryAfterFail = 5 * time.Minute
	tutorialsFetchTimeout   = 5 * time.Second
	tutorialsCatalogMaxSize = 1 << 20
	tutorialsPosterMaxSize  = 2 << 20
)

type tutorialChapter struct {
	T     float64 `json:"t"`
	Title string  `json:"title"`
}

type tutorialEntry struct {
	Slug      string            `json:"slug"`
	Title     string            `json:"title"`
	Summary   string            `json:"summary,omitempty"`
	Path      string            `json:"path,omitempty"`
	YouTubeID *string           `json:"youtube_id"`
	Seconds   *float64          `json:"seconds"`
	Poster    *string           `json:"poster"`
	Chapters  []tutorialChapter `json:"chapters,omitempty"`
	URL       string            `json:"url,omitempty"`
	Next      *string           `json:"next,omitempty"`
}

type tutorialPath struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Blurb string `json:"blurb"`
}

type tutorialCatalog struct {
	Version   int             `json:"version"`
	Paths     []tutorialPath  `json:"paths"`
	Tutorials []tutorialEntry `json:"tutorials"`
	Demos     []tutorialEntry `json:"demos"`
}

type tutorialPoster struct {
	data        []byte
	contentType string
}

// tutorialsCache holds the last good catalog and the posters fetched so far.
type tutorialsCache struct {
	mu        sync.Mutex
	catalog   *tutorialCatalog
	fetchedAt time.Time
	failedAt  time.Time
	// inflight is closed when the refresh in progress finishes; nil when none.
	inflight chan struct{}
	posters  map[string]tutorialPoster
}

// tutorialsClient is the outbound client, replaceable in tests. A dedicated
// client with its own timeout; a nil Transport is http.DefaultTransport.
func (s *Server) tutorialsClient() *http.Client {
	if s.tutorialsHTTP != nil {
		return s.tutorialsHTTP
	}
	return &http.Client{Timeout: tutorialsFetchTimeout}
}

func (s *Server) fetchTutorialBytes(ctx context.Context, url string, limit int64) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, tutorialsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	// Never follow a redirect: the origin check is on the URL asked for, and a
	// redirect would let an allowed getpad.dev URL send this server anywhere
	// (codex round 1). A 3xx is then just a non-200, and the fetch fails.
	client := *s.tutorialsClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("tutorials: %s answered %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("tutorials: %s is larger than %d bytes", url, limit)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// tutorialCatalogNow returns the catalog, refreshing it when it is older than
// the TTL. One refresh runs at a time and never holds the lock while it waits
// on the network (codex round 1): a caller with a copy in hand gets that copy
// at once, and only a caller with NO copy waits for the refresh in progress,
// bounded by its own request context. A failed refresh keeps the last good
// copy; with none, the result is nil and the client falls back to links.
// Failures are not retried more often than tutorialsRetryAfterFail, so an
// outage does not cost every request a timeout.
func (s *Server) tutorialCatalogNow(ctx context.Context) *tutorialCatalog {
	c := &s.tutorials
	c.mu.Lock()
	now := time.Now()
	fresh := c.catalog != nil && now.Sub(c.fetchedAt) < tutorialsTTL
	backingOff := !c.failedAt.IsZero() && now.Sub(c.failedAt) < tutorialsRetryAfterFail
	if fresh || backingOff {
		cat := c.catalog
		c.mu.Unlock()
		return cat
	}
	wait := c.inflight
	if wait == nil {
		wait = make(chan struct{})
		c.inflight = wait
		go s.refreshTutorialCatalog(wait)
	}
	cat := c.catalog
	c.mu.Unlock()
	if cat != nil {
		return cat // stale but usable; the refresh lands for the next caller
	}
	select {
	case <-wait:
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.catalog
}

// refreshTutorialCatalog fetches the catalog and publishes the result, then
// closes done. It runs detached from any one request, so a client that goes
// away does not cancel the refresh everyone else is waiting on.
func (s *Server) refreshTutorialCatalog(done chan struct{}) {
	body, _, err := s.fetchTutorialBytes(context.Background(), tutorialsCatalogURL, tutorialsCatalogMaxSize)
	var cat tutorialCatalog
	if err == nil {
		err = json.Unmarshal(body, &cat)
	}
	if err == nil && cat.Version != 1 {
		err = fmt.Errorf("tutorials: catalog version %d is not 1", cat.Version)
	}
	c := &s.tutorials
	c.mu.Lock()
	if err != nil {
		c.failedAt = time.Now()
	} else {
		c.catalog = &cat
		c.fetchedAt = time.Now()
		c.failedAt = time.Time{}
	}
	c.inflight = nil
	c.mu.Unlock()
	close(done)
}

func tutorialPosterPath(slug string) string {
	return "/api/v1/tutorials/posters/" + slug
}

// sameOriginEntries rewrites each poster to this server's proxy path.
func sameOriginEntries(in []tutorialEntry) []tutorialEntry {
	out := make([]tutorialEntry, len(in))
	for i, e := range in {
		if e.Poster != nil && *e.Poster != "" {
			p := tutorialPosterPath(e.Slug)
			e.Poster = &p
		} else {
			e.Poster = nil
		}
		out[i] = e
	}
	return out
}

// handleListTutorials: GET /tutorials. Cloud only (404 on a self-hosted
// server). {available, paths, tutorials, demos}; available=false when the
// catalog could not be fetched and no earlier copy exists, and the client then
// links to getpad.dev/learn.
func (s *Server) handleListTutorials(w http.ResponseWriter, r *http.Request) {
	if !s.cloudMode {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	cat := s.tutorialCatalogNow(r.Context())
	if cat == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false, "paths": []tutorialPath{}, "tutorials": []tutorialEntry{}, "demos": []tutorialEntry{},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"paths":     cat.Paths,
		"tutorials": sameOriginEntries(cat.Tutorials),
		"demos":     sameOriginEntries(cat.Demos),
	})
}

var errTutorialPosterOrigin = errors.New("tutorials: poster is not on getpad.dev")

// handleTutorialPoster: GET /tutorials/posters/{slug}. Cloud only. Serves the
// poster the catalog names for slug, fetched once from getpad.dev and kept in
// memory. Only an https://www.getpad.dev/ image is ever fetched.
func (s *Server) handleTutorialPoster(w http.ResponseWriter, r *http.Request) {
	if !s.cloudMode {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	slug := chi.URLParam(r, "slug")
	cat := s.tutorialCatalogNow(r.Context())
	var src string
	if cat != nil {
		for _, e := range append(append([]tutorialEntry{}, cat.Tutorials...), cat.Demos...) {
			if e.Slug == slug && e.Poster != nil {
				src = *e.Poster
				break
			}
		}
	}
	if src == "" {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}

	c := &s.tutorials
	c.mu.Lock()
	p, ok := c.posters[src]
	c.mu.Unlock()
	if !ok {
		data, ctype, err := s.fetchTutorialPoster(r.Context(), src)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_unavailable", "Poster unavailable")
			return
		}
		p = tutorialPoster{data: data, contentType: ctype}
		c.mu.Lock()
		if c.posters == nil {
			c.posters = map[string]tutorialPoster{}
		}
		c.posters[src] = p
		c.mu.Unlock()
	}
	w.Header().Set("Content-Type", p.contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(p.data)
}

func (s *Server) fetchTutorialPoster(ctx context.Context, src string) ([]byte, string, error) {
	if !strings.HasPrefix(src, tutorialsPosterOrigin) {
		return nil, "", errTutorialPosterOrigin
	}
	data, ctype, err := s.fetchTutorialBytes(ctx, src, tutorialsPosterMaxSize)
	if err != nil {
		return nil, "", err
	}
	switch ctype = strings.ToLower(strings.TrimSpace(strings.Split(ctype, ";")[0])); ctype {
	case "image/jpeg", "image/png", "image/webp":
		return data, ctype, nil
	}
	return nil, "", fmt.Errorf("tutorials: poster content type %q", ctype)
}
