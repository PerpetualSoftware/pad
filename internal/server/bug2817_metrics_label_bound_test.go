package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// labelValues returns the distinct values of one label across a metric family.
func labelValues(t *testing.T, m *metrics.Metrics, family, label string) []string {
	t.Helper()
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, mm := range f.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == label {
					seen[lp.GetValue()] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// BUG-2817: N distinct caller-invented tool names, sent through the REAL /mcp
// handler chain, add ONE label value between them. The control is a known name
// in the same run, which must keep its own value; a bound that mapped
// everything to "unknown" fails it. The audit ROW keeps what the caller sent.
func TestMCPToolMetricsLabel_IsBoundedByTheKnownNames(t *testing.T) {
	srv, user, bearer := auditedMCPServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	srv.mcpCallNameKnown = (func(n string) bool { return n == "pad_item" || n == "tools/list" })

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}

	const n = 25
	post(`{"jsonrpc":"2.0","id":0,"method":"tools/call","params":{"name":"pad_item","arguments":{}}}`)
	for i := 1; i <= n; i++ {
		post(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"bogus_%d","arguments":{}}}`, i, i))
	}
	// A caller-invented JSON-RPC METHOD is the other half of the label.
	post(`{"jsonrpc":"2.0","id":99,"method":"invented/method"}`)

	want := []string{"pad_item", unknownToolLabel} // sorted
	for _, fam := range []string{"pad_mcp_tool_calls_total", "pad_mcp_tool_call_duration_seconds"} {
		if got := labelValues(t, m, fam, "tool"); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s tool labels = %v after %d invented names, want %v", fam, got, n+1, want)
		}
	}

	// The audit row is where the full value belongs, and it is kept.
	rows := waitForAuditRows(t, srv, user.ID, n+2)
	found := false
	for _, r := range rows {
		if r.ToolName == "bogus_7" {
			found = true
		}
	}
	if !found {
		t.Errorf("the audit row for bogus_7 lost its tool name; the bound applies to the label only")
	}
}

// With no predicate wired, every name is "unknown": an unwired server cannot
// mint caller-chosen series.
func TestMCPToolMetricsLabel_UnwiredIsUnknown(t *testing.T) {
	s := &Server{metrics: metrics.New()}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	s.recordMCPCallMetrics("pad_item", "ok", "user-1", 0, r, http.StatusOK)
	s.recordMCPCallMetrics("anything", "ok", "user-1", 0, r, http.StatusOK)
	for _, fam := range []string{"pad_mcp_tool_calls_total", "pad_mcp_tool_call_duration_seconds"} {
		if got := labelValues(t, s.metrics, fam, "tool"); strings.Join(got, ",") != unknownToolLabel {
			t.Errorf("%s unwired tool labels = %v, want [%s]", fam, got, unknownToolLabel)
		}
	}
}

// BUG-2817, the unauthenticated half: MetricsMiddleware runs ahead of auth,
// and Go's server accepts any token as a method. N invented methods add ONE
// label value; the standard methods keep theirs.
func TestHTTPMetricsMethodLabel_IsBounded(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(MetricsMiddleware(m))
	r.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) {})

	standard := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace}
	for _, method := range standard {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/x", nil))
	}
	const n = 25
	for i := 0; i < n; i++ {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(fmt.Sprintf("M%d", i), "/x", nil))
	}

	// Every standard method keeps its own value, and the invented ones share one.
	wantSet := append(append([]string{}, standard...), "OTHER")
	sort.Strings(wantSet)
	want := strings.Join(wantSet, ",")
	for _, fam := range []string{"pad_http_requests_total", "pad_http_request_duration_seconds", "pad_http_response_size_bytes"} {
		if got := strings.Join(labelValues(t, m, fam, "method"), ","); got != want {
			t.Errorf("%s method labels = %s after %d invented methods, want %s", fam, got, n, want)
		}
	}
}
