package webhooks

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3409: on the owner path, 408, 425 and 429 mean "come back later" and
// are retried; a plain 400 or 404 stays permanent and is sent once.
func TestTask3409_OwnerHookRetriesTheComeBackLaterStatuses(t *testing.T) {
	for _, tc := range []struct {
		status   int
		attempts int32
	}{
		{408, maxDeliveryAttempts}, {425, maxDeliveryAttempts}, {429, maxDeliveryAttempts},
		{503, maxDeliveryAttempts},
		{400, 1}, {404, 1},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			var attempts int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(tc.status)
			}))
			defer ts.Close()
			store := newMockStore([]models.Webhook{{ID: "hook-1", WorkspaceID: "ws-1", URL: ts.URL, Events: `["*"]`, Active: true}})
			d := NewDispatcher(store)
			d.SkipSSRF = true
			d.retryBackoff = 0
			out, err := d.DeliverEvent(Delivery{WorkspaceID: "ws-1", EventID: "e1", Event: "item.created", Payload: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if got := atomic.LoadInt32(&attempts); got != tc.attempts {
				t.Errorf("%d attempts, want %d", got, tc.attempts)
			}
			transient := tc.attempts == maxDeliveryAttempts
			if out.Retryable() != transient {
				t.Errorf("Retryable() = %v, want %v (outcome %+v)", out.Retryable(), transient, out)
			}
		})
	}
}

func TestTask3409_RetryableStatus(t *testing.T) {
	for code, want := range map[int]bool{
		200: false, 301: false, 400: false, 401: false, 403: false, 404: false, 410: false, 422: false,
		408: true, 425: true, 429: true, 500: true, 502: true, 503: true, 599: true,
	} {
		if got := RetryableStatus(code); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", code, got, want)
		}
	}
}
