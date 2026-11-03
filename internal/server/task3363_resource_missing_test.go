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
// and that is safe only because the grant binds the audience (fosite replaces
// the requested audience with the stored one): a code granted
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

// decideTarget posts a consent decision carrying one target parameter (key is
// "resource" or "audience") and returns the redirect's query.
func decideTarget(t *testing.T, srv *Server, sess oauthSession, key, value string) url.Values {
	t.Helper()
	form := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"code_challenge": {s256Challenge("verifier-3363-aud-abcdefghijklmnopqrstuvwxyz0123456789")}, "code_challenge_method": {"S256"},
		"scope": {"pad:read"}, "state": {"state-3363-aud-01"}, "decision": {"approve"},
		"csrf_token": {sess.csrfTok}, "capability_tier": {"read"}, "allowed_workspaces": {"*"},
		key: {value},
	}
	rr := postFormWithCookie(srv, "/oauth/authorize/decide", form, sess.sessionToken, sess.csrfTok)
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if loc == nil {
		t.Fatalf("decide with %s=%s: %d, no redirect", key, value, rr.Code)
	}
	return loc.Query()
}

func resourceMissingCount(t *testing.T, m *metrics.Metrics, label string) float64 {
	t.Helper()
	var pb io_prometheus_client.Metric
	if err := m.OAuthResourceMissingTotal.WithLabelValues(label).Write(&pb); err != nil {
		t.Fatal(err)
	}
	return pb.GetCounter().GetValue()
}

// TASK-3363 phase 2, dev lead ruling: audience= without resource= is NOT yet
// refused (phase 1 never counted it). It is served, counted under its own
// label, and enforced the release after v0.20.0.
func TestTASK3363_AudienceOnlyIsServedAndCountedUnderItsOwnLabel(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	sess := newOAuthSession(t, srv)

	q := decideTarget(t, srv, sess, "audience", testCanonicalAudience)
	if q.Get("code") == "" {
		t.Fatalf("decide with audience= only was not served: %v", q)
	}
	if got := resourceMissingCount(t, m, "decide_audience_only"); got != 1 {
		t.Fatalf("decide_audience_only = %v, want 1", got)
	}
	if got := resourceMissingCount(t, m, "decide"); got != 0 {
		t.Fatalf("an audience-only request was counted as a neither-key omission: decide = %v", got)
	}

	// The authorize step is counted too, and is not refused with invalid_target.
	aq := url.Values{
		"client_id": {sess.clientID}, "response_type": {"code"}, "redirect_uri": {"https://app.test/cb"},
		"scope": {"pad:read"}, "code_challenge": {s256Challenge("verifier-3363-abcdefghijklmnopqrstuvwxyz0123456789")},
		"code_challenge_method": {"S256"}, "state": {"state-3363-02"}, "audience": {testCanonicalAudience},
	}
	// newOAuthSession already hit /oauth/authorize with audience= to read the
	// CSRF cookie, so count the delta.
	before := resourceMissingCount(t, m, "authorize_audience_only")
	rr := doRequestWithCookie(srv, "GET", "/oauth/authorize?"+aq.Encode(), nil, sess.sessionToken)
	if loc, _ := url.Parse(rr.Header().Get("Location")); loc != nil && loc.Query().Get("error") == "invalid_target" {
		t.Fatalf("authorize with audience= only was refused with invalid_target; that is the next release: %s", rr.Header().Get("Location"))
	}
	if got := resourceMissingCount(t, m, "authorize_audience_only") - before; got != 1 {
		t.Fatalf("authorize_audience_only rose by %v, want 1", got)
	}

	// A request that names resource= is not an audience-only request.
	if q := decideTarget(t, srv, sess, "resource", testCanonicalAudience); q.Get("code") == "" {
		t.Fatalf("decide with resource= was not served: %v", q)
	}
	if got := resourceMissingCount(t, m, "decide_audience_only"); got != 1 {
		t.Fatalf("a resource= request moved decide_audience_only to %v", got)
	}
}

// The condition on that ruling: audience= is held to the same allowed set as
// resource=. A value resource= refuses, audience= refuses.
func TestTASK3363_AudienceIsValidatedLikeResource(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	sess := newOAuthSession(t, srv)
	for _, bad := range []string{"https://evil.example/mcp", testCanonicalAudience + "/other", "not a url"} {
		viaResource := decideTarget(t, srv, sess, "resource", bad)
		viaAudience := decideTarget(t, srv, sess, "audience", bad)
		if viaResource.Get("code") != "" {
			t.Fatalf("resource=%q was granted a code; the premise of this test is wrong", bad)
		}
		if viaAudience.Get("code") != "" {
			t.Errorf("audience=%q was granted a code that resource= refuses", bad)
		}
		if viaAudience.Get("error") == "" {
			t.Errorf("audience=%q: no error in the redirect: %v", bad, viaAudience)
		}
	}
}

// An installed app's delegated sign-in goes through the same decide handler
// and gets no code without resource= (population row: decideAppConsent). The
// refusal is invalid_request, not invalid_target: the default injected for the
// missing resource is the /mcp canonical, which an install client's audience
// (the app API only) does not hold, so fosite refuses the request before the
// phase-2 check runs. Either way no code is issued; with resource the code is
// served.
func TestTASK3363_DelegatedSignInWithoutResourceIsRefused(t *testing.T) {
	f := delegatedFixture(t, "t3363-deleg", "write")
	params := f.authorizeParams()

	rr := f.decide(t, params, "approve", "write")
	if code, oauthErr := codeFrom(rr); code == "" {
		t.Fatalf("decide WITH resource: no code (%q); the control is broken", oauthErr)
	}

	params.Del("resource")
	rr = f.decide(t, params, "approve", "write")
	code, oauthErr := codeFrom(rr)
	if code != "" || oauthErr == "" {
		t.Fatalf("decide WITHOUT resource: code=%q error=%q, want no code and an error", code, oauthErr)
	}
}

// The audience-only log names the client however it identifies itself (form
// or HTTP Basic), and an oversized client id is cut, not stored whole in the
// limiter map. Not parallel: it swaps the default logger.
func TestTASK3363_AudienceOnlyNoteNamesTheClientAndBoundsTheID(t *testing.T) {
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
		srv.resourceMissingLast = nil
		req := httptest.NewRequest("GET", "/oauth/authorize", nil)
		req.Form = url.Values{"audience": {testCanonicalAudience}}
		c.build(req)
		srv.noteAudienceOnly(req, "authorize")
		line := buf.String()
		for _, want := range []string{"endpoint=authorize_audience_only", "client_id=" + clientID} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: log %q lacks %q", c.name, line, want)
			}
		}
		if strings.Contains(line, `client_name=""`) {
			t.Errorf("%s: the registered client name was not logged: %q", c.name, line)
		}
	}

	// An oversized id is cut to the bound in both the log and the limiter key.
	buf.Reset()
	srv.resourceMissingLast = nil
	huge := strings.Repeat("x", 4*resourceNoteMaxClientID)
	req := httptest.NewRequest("GET", "/oauth/authorize", nil)
	req.Form = url.Values{"audience": {testCanonicalAudience}, "client_id": {huge}}
	srv.noteAudienceOnly(req, "authorize")
	if strings.Contains(buf.String(), huge) {
		t.Errorf("the oversized client id was logged whole")
	}
	for key := range srv.resourceMissingLast {
		if len(key) > len("audience-only:")+resourceNoteMaxClientID {
			t.Errorf("limiter key of %d bytes stored", len(key))
		}
	}
}
