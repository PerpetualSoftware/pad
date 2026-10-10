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

// TASK-3363 phase 2: a consent decision that omits the RFC 8707 resource
// parameter is REFUSED with invalid_target, and counted. With resource it is
// served and nothing is counted.
func TestTASK3363_MissingResourceIsRefusedAndCounted(t *testing.T) {
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

	if _, code := mintWithResource(t, srv, sess, testCanonicalAudience); code != 200 {
		t.Fatalf("mint with resource: %d", code)
	}
	if count("decide") != 0 || count("token") != 0 {
		t.Fatalf("a request WITH resource was counted: decide=%v token=%v", count("decide"), count("token"))
	}

	// Without: refused at the decision, so no code and no token exchange.
	if _, code := mintWithResource(t, srv, sess, ""); code == 200 {
		t.Fatal("mint without resource was served; phase 2 refuses it")
	}
	if count("decide") != 1 || count("token") != 0 {
		t.Fatalf("a decision WITHOUT resource: decide=%v token=%v, want 1 and 0", count("decide"), count("token"))
	}
}

// The authorize step refuses too, at the client's redirect_uri, and counts.
func TestTASK3363_AuthorizeWithoutResourceIsRefusedAndCounted(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	sess := newOAuthSession(t, srv)
	q := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"scope": {"pad:read"}, "code_challenge": {s256Challenge("verifier-3363-abcdefghijklmnopqrstuvwxyz0123456789")},
		"code_challenge_method": {"S256"}, "state": {"state-3363-01"},
	}
	rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+q.Encode(), nil, sess.sessionToken)
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if (rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound) || loc == nil || loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("authorize without resource: %d %s, want a redirect with error=invalid_target", rr.Code, rr.Header().Get("Location"))
	}
	var pb io_prometheus_client.Metric
	if err := m.OAuthResourceMissingTotal.WithLabelValues("authorize").Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetCounter().GetValue(); got != 1 {
		t.Fatalf("authorize without resource counted %v, want 1", got)
	}
}

// The token endpoint is NOT refused (RFC 8707 makes resource optional there),
// and that is safe only because the grant binds the audience: a code granted
// for the ChatGPT resource, exchanged WITHOUT resource (so the request is
// defaulted to /mcp's canonical), still yields a token bound to the ChatGPT
// resource, refused at /mcp. The exchange is counted.
func TestTASK3363_TokenExchangeWithoutResourceKeepsTheGrantedAudience(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	sess := newOAuthSession(t, srv)

	verifier := "verifier-3363-tok-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	form := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"code_challenge": {s256Challenge(verifier)}, "code_challenge_method": {"S256"},
		"scope": {"pad:read"}, "state": {"state-3363-tok-01"}, "decision": {"approve"},
		"csrf_token": {sess.csrfTok}, "capability_tier": {"read"}, "allowed_workspaces": {"*"},
		"resource": {testChatGPTResource},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sess.sessionToken, sess.csrfTok)
	cb, _ := url.Parse(rr.Header().Get("Location"))
	code := cb.Query().Get("code")
	if code == "" {
		t.Fatalf("decide with resource: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	trr := postOAuthForm(srv, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {sess.clientID},
		"redirect_uri": {"https://app.test/cb"}, "code_verifier": {verifier},
	})
	if trr.Code != http.StatusOK {
		t.Fatalf("token exchange without resource: %d %s (RFC 8707: optional here)", trr.Code, trr.Body.String())
	}
	var resp map[string]any
	parseJSON(t, trr, &resp)
	tok, _ := resp["access_token"].(string)
	if got := atMount(srv, testChatGPTResource, tok); got != http.StatusOK {
		t.Fatalf("the token is not served at its granted (ChatGPT) mount: %d", got)
	}
	if got := atMount(srv, "", tok); got != http.StatusUnauthorized {
		t.Fatalf("the default widened the grant: the ChatGPT-granted token is served at /mcp (%d)", got)
	}
	var pb io_prometheus_client.Metric
	if err := m.OAuthResourceMissingTotal.WithLabelValues("token").Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetCounter().GetValue(); got != 1 {
		t.Fatalf("the token exchange without resource counted %v, want 1", got)
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
