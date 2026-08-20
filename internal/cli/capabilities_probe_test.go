package cli

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// capsHandler serves /api/v1/server/capabilities with a swappable status/body
// and counts hits, so a test can assert BOTH the verdict and whether a probe
// was (re)issued.
type capsHandler struct {
	status atomic.Int32
	body   atomic.Value // string
	hits   atomic.Int32
}

func (h *capsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/server/capabilities" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.hits.Add(1)
	w.WriteHeader(int(h.status.Load()))
	if b, _ := h.body.Load().(string); b != "" {
		_, _ = w.Write([]byte(b))
	}
}

func TestCollectionNotFoundIsAuthoritative_Definitive_CachesVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"resolves", http.StatusOK, `{"collection_resolution":true}`, true},
		{"advertises false", http.StatusOK, `{"collection_resolution":false}`, false},
		{"legacy 404", http.StatusNotFound, "not found", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &capsHandler{}
			h.status.Store(int32(tc.status))
			h.body.Store(tc.body)
			ts := httptest.NewServer(h)
			defer ts.Close()
			c := NewClientFromURL(ts.URL)

			if got := c.CollectionNotFoundIsAuthoritative(); got != tc.want {
				t.Fatalf("first call = %v, want %v", got, tc.want)
			}
			// A definitive verdict is cached: the second call must NOT re-probe.
			if got := c.CollectionNotFoundIsAuthoritative(); got != tc.want {
				t.Fatalf("second call = %v, want %v", got, tc.want)
			}
			if h.hits.Load() != 1 {
				t.Fatalf("expected exactly one probe (definitive verdict cached), got %d", h.hits.Load())
			}
		})
	}
}

func TestCollectionNotFoundIsAuthoritative_Transient_FailsClosedAndReprobes(t *testing.T) {
	// Codex r2 P1: a transient probe failure (here a 5xx) must NOT be cached as
	// "legacy" — caching it would permanently re-enable the alias retry and let
	// a write bypass the archived/hidden protection on a resolving server.
	h := &capsHandler{}
	h.status.Store(http.StatusInternalServerError)
	h.body.Store("boom")
	ts := httptest.NewServer(h)
	defer ts.Close()
	c := NewClientFromURL(ts.URL)

	// Fail CLOSED: an indeterminate probe trusts the not-found (no retry).
	if got := c.CollectionNotFoundIsAuthoritative(); !got {
		t.Fatalf("transient probe must fail closed (true), got false")
	}
	// And it must NOT be cached: once the blip clears, the real verdict wins.
	h.status.Store(http.StatusOK)
	h.body.Store(`{"collection_resolution":false}`)
	if got := c.CollectionNotFoundIsAuthoritative(); got {
		t.Fatalf("after the blip cleared the verdict must be re-probed (false), got true")
	}
	if h.hits.Load() != 2 {
		t.Fatalf("expected a re-probe after the transient failure, got %d probes", h.hits.Load())
	}
}
