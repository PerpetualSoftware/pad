package billing

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newStub starts a test HTTP server and returns a CloudClient pointed at it.
// Callers supply the handler to control the response.
func newStub(t *testing.T, h http.HandlerFunc) (*CloudClient, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return NewCloudClient(ts.URL, "test-secret"), ts
}

func TestCancelCustomer_HappyPath_SendsCorrectRequest(t *testing.T) {
	var (
		gotMethod      string
		gotPath        string
		gotContentType string
		gotBody        map[string]string
	)
	client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"subscriptions_cancelled":2}`))
	})

	if err := client.CancelCustomer("cus_abc"); err != nil {
		t.Fatalf("expected nil error on 200, got %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/billing/cancel-customer" {
		t.Errorf("expected /billing/cancel-customer, got %s", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("expected application/json content type, got %s", gotContentType)
	}
	if gotBody["customer_id"] != "cus_abc" {
		t.Errorf("expected customer_id=cus_abc, got %q", gotBody["customer_id"])
	}
	if gotBody["cloud_secret"] != "test-secret" {
		t.Errorf("expected cloud_secret=test-secret, got %q", gotBody["cloud_secret"])
	}
}

// Each real pad-cloud failure status maps to a SidecarError carrying that
// status. Together these cover every non-2xx shape pad-cloud returns:
//   - 400 on malformed request or non-cus_ customer_id
//   - 403 on cloud_secret mismatch
//   - 500 on internal / Stripe failure
//   - 503 when Stripe is not configured
// All of them must produce a SidecarError — the handler treats every one
// as "abort the delete", regardless of bucket.
func TestCancelCustomer_NonOK_ReturnsSidecarError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		bodyHas string
	}{
		{"400_bad_request", http.StatusBadRequest, `{"error":"customer_id must start with 'cus_'"}`, "customer_id must start with"},
		{"403_wrong_secret", http.StatusForbidden, `{"error":"Forbidden"}`, "Forbidden"},
		{"500_stripe_failure", http.StatusInternalServerError, `{"error":"Failed to cancel subscription"}`, "Failed to cancel"},
		{"503_not_configured", http.StatusServiceUnavailable, `{"error":"Stripe billing not configured"}`, "not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			err := client.CancelCustomer("cus_abc")
			if err == nil {
				t.Fatalf("expected error on %d, got nil", tc.status)
			}

			var se *SidecarError
			if !errors.As(err, &se) {
				t.Fatalf("expected *SidecarError, got %T: %v", err, err)
			}
			if se.Status != tc.status {
				t.Errorf("expected status %d, got %d", tc.status, se.Status)
			}
			if !strings.Contains(se.Body, tc.bodyHas) {
				t.Errorf("expected body to include %q, got %q", tc.bodyHas, se.Body)
			}
		})
	}
}

func TestCancelCustomer_TransportFailure_NotSidecarError(t *testing.T) {
	// Point at a URL nothing's listening on; use a short client timeout so
	// the test doesn't hang if the OS accepts the connect.
	client := NewCloudClient("http://127.0.0.1:1", "test-secret")
	client.http.Timeout = 500 * time.Millisecond

	err := client.CancelCustomer("cus_abc")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}

	// A transport failure must NOT be a *SidecarError — callers use this to
	// distinguish "retry" (no status from upstream) from "upstream spoke".
	var se *SidecarError
	if errors.As(err, &se) {
		t.Errorf("transport failure must not be SidecarError, got %v", err)
	}
}

func TestCancelCustomer_EmptyCustomerID_ReturnsError(t *testing.T) {
	called := false
	client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	err := client.CancelCustomer("")
	if err == nil {
		t.Fatal("expected error for empty customer_id, got nil")
	}
	if called {
		t.Error("expected no HTTP call for empty customer_id")
	}
}

func TestCancelCustomer_NilClient_ReturnsError(t *testing.T) {
	var c *CloudClient
	if err := c.CancelCustomer("cus_abc"); err == nil {
		t.Fatal("expected error from nil receiver, got nil")
	}
}

func TestCancelCustomer_UnconfiguredClient_ReturnsError(t *testing.T) {
	c := NewCloudClient("", "")
	err := c.CancelCustomer("cus_abc")
	if err == nil {
		t.Fatal("expected error for unconfigured client, got nil")
	}
	if strings.Contains(err.Error(), "dial") {
		t.Errorf("expected config error, got transport error: %v", err)
	}
}

func TestResolveOutboundSecret(t *testing.T) {
	cases := []struct {
		name         string
		explicit     string
		inboundList  string
		want         string
		wantEmpty    bool
	}{
		{
			name:        "explicit wins over inbound",
			explicit:    "explicit-key",
			inboundList: "new,old",
			want:        "explicit-key",
		},
		{
			name:        "explicit with whitespace is trimmed",
			explicit:    "  pinned-key  ",
			inboundList: "new,old",
			want:        "pinned-key",
		},
		{
			name:        "falls back to last inbound during rotation (new,old)",
			explicit:    "",
			inboundList: "new-key,old-key",
			want:        "old-key",
		},
		{
			name:        "single inbound value is used",
			explicit:    "",
			inboundList: "only-key",
			want:        "only-key",
		},
		{
			name:        "skips trailing empty entries",
			explicit:    "",
			inboundList: "new-key,old-key,,",
			want:        "old-key",
		},
		{
			name:        "skips whitespace-only entries",
			explicit:    "",
			inboundList: "new-key,   ,old-key",
			want:        "old-key",
		},
		{
			name:        "explicit set but inbound empty → explicit",
			explicit:    "only-explicit",
			inboundList: "",
			want:        "only-explicit",
		},
		{
			name:        "both empty → empty result (caller should fail startup)",
			explicit:    "",
			inboundList: "",
			wantEmpty:   true,
		},
		{
			name:        "both whitespace → empty result",
			explicit:    "   ",
			inboundList: ", , ,",
			wantEmpty:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOutboundSecret(tc.explicit, tc.inboundList)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("ResolveOutboundSecret(%q, %q) = %q, want %q",
					tc.explicit, tc.inboundList, got, tc.want)
			}
		})
	}
}

func TestCancelCustomer_LargeResponseBody_DoesNotReadPastCap(t *testing.T) {
	// A broken sidecar or misrouted proxy can stream us an enormous payload.
	// We cap the read at maxResponseBody so we never allocate MBs of error
	// body. Build a body bigger than the cap and verify the client still
	// returns a SidecarError with a truncated-but-bounded Body.
	big := strings.Repeat("X", maxResponseBody*2)
	client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(big))
	})

	err := client.CancelCustomer("cus_abc")
	var se *SidecarError
	if !errors.As(err, &se) {
		t.Fatalf("expected SidecarError, got %T", err)
	}
	if len(se.Body) > maxResponseBody {
		t.Errorf("body was %d bytes, expected <= %d (cap)", len(se.Body), maxResponseBody)
	}
}
