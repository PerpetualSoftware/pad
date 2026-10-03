package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// TASK-3363 phase 1: a request that omits the RFC 8707 resource parameter is
// still defaulted to the canonical audience (phase 2 refuses it), and each
// one is now counted per endpoint, so the refusal can be measured first.
func TestTASK3363_MissingResourceIsCountedPerEndpoint(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	count := func(endpoint string) float64 {
		var pb io_prometheus_client.Metric
		if err := m.OAuthResourceMissingTotal.WithLabelValues(endpoint).Write(&pb); err != nil {
			t.Fatal(err)
		}
		return pb.GetCounter().GetValue()
	}
	sess := newOAuthSession(t, srv)

	// With resource on both legs: nothing is counted.
	if _, code := mintWithResource(t, srv, sess, testCanonicalAudience); code != 200 {
		t.Fatalf("mint with resource: %d", code)
	}
	if count("decide") != 0 || count("token") != 0 {
		t.Fatalf("a request WITH resource was counted: decide=%v token=%v", count("decide"), count("token"))
	}

	// Without: still served (phase 1 changes no behaviour), and counted at
	// both the consent decision and the token exchange.
	if _, code := mintWithResource(t, srv, sess, ""); code != 200 {
		t.Fatalf("mint without resource: %d (phase 1 must not refuse)", code)
	}
	if count("decide") != 1 || count("token") != 1 {
		t.Fatalf("a request WITHOUT resource: decide=%v token=%v, want 1 and 1", count("decide"), count("token"))
	}
}

// The authorize step (the consent page) counts too.
func TestTASK3363_AuthorizeWithoutResourceIsCounted(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	sess := newOAuthSession(t, srv)
	q := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"scope": {"pad:read"}, "code_challenge": {s256Challenge("verifier-3363-abcdefghijklmnopqrstuvwxyz0123456789")},
		"code_challenge_method": {"S256"}, "state": {"state-3363-01"},
	}
	if rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sess.sessionToken); rr.Code != 200 {
		t.Fatalf("consent page without resource: %d (phase 1 must not refuse)", rr.Code)
	}
	var pb io_prometheus_client.Metric
	if err := m.OAuthResourceMissingTotal.WithLabelValues("authorize").Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetCounter().GetValue(); got != 1 {
		t.Fatalf("authorize without resource counted %v, want 1", got)
	}
}

// The log line is half the phase-1 signal: it must name the endpoint and the
// client, including a client that sends its id by HTTP Basic auth. Not
// parallel: it swaps the default logger.
func TestTASK3363_MissingResourceIsLoggedWithTheClient(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := twoResourceOAuthServer(t)
	clientID := registerTestClient(t, srv, "https://app.test/cb")
	for _, c := range []struct {
		name  string
		build func(*http.Request)
	}{
		{"form client_id", func(r *http.Request) { r.Form.Set("client_id", clientID) }},
		{"HTTP Basic", func(r *http.Request) { r.SetBasicAuth(url.QueryEscape(clientID), "") }},
	} {
		buf.Reset()
		srv.resourceMissingLast = nil // each case is a first sighting
		req := httptest.NewRequest("POST", "/oauth/token", nil)
		req.Form = url.Values{}
		c.build(req)
		srv.noteResourceMissing(req, "token")
		line := buf.String()
		for _, want := range []string{"endpoint=token", "client_id=" + clientID, "client_name="} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: log %q lacks %q", c.name, line, want)
			}
		}
		if strings.Contains(line, `client_name=""`) {
			t.Errorf("%s: the registered client name was not logged: %q", c.name, line)
		}
	}
}

// One log line per client per hour, while the counter takes every request:
// a popular client that omits resource must not flood the log.
func TestTASK3363_WarningIsRateLimitedPerClient(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := twoResourceOAuthServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	clock := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	srv.resourceMissingNow = func() time.Time { return clock }
	note := func(client string) {
		req := httptest.NewRequest("POST", "/oauth/token", nil)
		req.Form = url.Values{"client_id": {client}}
		srv.noteResourceMissing(req, "token")
	}
	lines := func() int { return strings.Count(buf.String(), "RFC 8707 resource parameter") }

	note("client-a")
	note("client-a")
	note("client-b")
	if got := lines(); got != 2 {
		t.Fatalf("within the hour: %d log lines, want 2 (one per client)", got)
	}
	var pb io_prometheus_client.Metric
	if err := m.OAuthResourceMissingTotal.WithLabelValues("token").Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetCounter().GetValue(); got != 3 {
		t.Fatalf("counter = %v, want 3 (every request)", got)
	}
	clock = clock.Add(resourceMissingWarnInterval)
	note("client-a")
	if got := lines(); got != 3 {
		t.Fatalf("after the hour: %d log lines, want 3", got)
	}
}
