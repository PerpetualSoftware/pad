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

func TestCancelCustomer_ClientError_4xxReturnsSidecarError(t *testing.T) {
	client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"customer_id must start with 'cus_'"}`))
	})

	err := client.CancelCustomer("cus_abc")
	if err == nil {
		t.Fatal("expected error on 400, got nil")
	}

	var se *SidecarError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SidecarError, got %T: %v", err, err)
	}
	if se.Status != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", se.Status)
	}
	if !strings.Contains(se.Body, "customer_id must start with") {
		t.Errorf("expected body to include upstream message, got %q", se.Body)
	}
	if !IsClientError(err) {
		t.Error("IsClientError must be true for 4xx")
	}
}

func TestCancelCustomer_Forbidden_403IsClientError(t *testing.T) {
	// pad-cloud returns 403 when the cloud_secret mismatch — which is NOT
	// a "customer gone" condition. The caller should still abort, but only
	// because 403 is classified as ops-misconfig (wrong secret), not because
	// of the log-and-continue path. The IsClientError bool is a routing
	// switch; callers that need finer granularity can inspect Status.
	client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
	})

	err := client.CancelCustomer("cus_abc")
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if !IsClientError(err) {
		t.Error("403 should be classified as client error")
	}
}

func TestCancelCustomer_ServerError_5xxIsNotClientError(t *testing.T) {
	client, _ := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Failed to cancel subscription"}`))
	})

	err := client.CancelCustomer("cus_abc")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}

	var se *SidecarError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SidecarError, got %T", err)
	}
	if se.Status != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", se.Status)
	}
	if IsClientError(err) {
		t.Error("IsClientError must be false for 5xx")
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
	if IsClientError(err) {
		t.Error("IsClientError must be false for transport errors")
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

func TestIsClientError_NonSidecarError(t *testing.T) {
	if IsClientError(errors.New("plain error")) {
		t.Error("IsClientError must be false for non-SidecarError")
	}
	if IsClientError(nil) {
		t.Error("IsClientError(nil) must be false")
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
