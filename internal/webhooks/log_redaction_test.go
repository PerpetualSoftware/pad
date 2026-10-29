package webhooks

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestWebhookLogsRedactCredentials(t *testing.T) {
	for _, mode := range []string{"network", "invalid URL", "app network"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			transportErr := errors.New("redirect https://other.example/error-credential?password=error-password")
			if mode == "app network" {
				d, _ := newAppTestDispatcher()
				d.DeliverAppEvent(&fakeAdmitter{}, &fakePoster{err: transportErr}, appDV)
			} else {
				d := NewDispatcher(newMockStore(nil))
				d.SkipSSRF = true
				d.client = &http.Client{Transport: &errRoundTripper{err: transportErr}}
				hook := models.Webhook{ID: "hook-id", WorkspaceID: "ws", URL: "https://url-user:url-password@example.com/path-credential?api_key=query-credential#fragment-credential", Secret: "webhook-secret-fixture"}
				if mode == "invalid URL" {
					hook.URL += "%invalid"
				}
				d.attemptDeliver(hook, []byte(`{}`))
			}
			for _, secret := range []string{"url-user", "url-password", "path-credential", "query-credential", "fragment-credential", "error-credential", "error-password", "webhook-secret-fixture"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("%s reached webhook logs", secret)
				}
			}
			if !strings.Contains(logs.String(), "webhook") {
				t.Error("missing webhook diagnostic")
			}
		})
	}
}
