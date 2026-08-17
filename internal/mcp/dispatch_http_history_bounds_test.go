package mcp

// BUG-2608 — `pad_item.action=history` was unbounded, and summary mode paid
// for content it discarded: the endpoint resolved every version by walking the
// item's whole reverse-patch chain, and the dispatcher then projected that
// away to metadata.
//
// Two independent claims, tested where each actually lives:
//   - the DEFAULT window is injected by the catalog action, so it reaches both
//     transports — asserted on the CLI args the action produces, which is also
//     the stdio half (BuildCLIArgs emits `--limit`).
//   - the HTTP dispatcher asks the server to SKIP resolution when the caller
//     did not ask for content — asserted on the request it builds.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func historyArgs(t *testing.T, input map[string]any) string {
	t.Helper()
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := actionItemHistory(context.Background(), input, env)
	if err != nil {
		t.Fatalf("actionItemHistory error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("error result: %s", textOf(res))
	}
	return strings.Join(disp.gotArgs, " ")
}

// A bare history call must be bounded. Asserting the CLI args covers the exec
// transport at the same time: the default only reaches stdio because
// BuildCLIArgs turns the param into the CLI's --limit flag.
func TestPadItemHistory_AppliesDefaultLimit(t *testing.T) {
	joined := historyArgs(t, map[string]any{"ref": "TASK-5"})
	want := "--limit " + strconv.Itoa(mcpItemHistoryDefaultLimit)
	if !strings.Contains(joined, want) {
		t.Errorf("cliArgs %q should carry the injected default %q — an unbounded "+
			"history is the bug", joined, want)
	}
}

func TestPadItemHistory_ClampsOversizedLimit(t *testing.T) {
	joined := historyArgs(t, map[string]any{"ref": "TASK-5", "limit": float64(99999)})
	want := "--limit " + strconv.Itoa(mcpItemHistoryMaxLimit)
	if !strings.Contains(joined, want) {
		t.Errorf("cliArgs %q should clamp to %q", joined, want)
	}
}

func TestPadItemHistory_HonorsInRangeLimit(t *testing.T) {
	joined := historyArgs(t, map[string]any{"ref": "TASK-5", "limit": float64(7)})
	if !strings.Contains(joined, "--limit 7") {
		t.Errorf("cliArgs %q should honor an in-range limit of 7", joined)
	}
}

// The HTTP half of the optimization. Summary mode is not merely a smaller
// response — it tells the server not to walk the patch chain at all. If the
// dispatcher stops sending it, the projection below still looks identical
// while the server goes back to resolving bodies nobody reads, which is the
// silent half of this bug.
func TestDispatchItemHistory_RequestsSummaryUnlessFullAsked(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input       map[string]any
		wantSummary bool
	}{
		{"default asks the server to skip resolution", map[string]any{
			"workspace": "docapp", "ref": "TASK-5",
		}, true},
		{"full=true must NOT skip it", map[string]any{
			"workspace": "docapp", "ref": "TASK-5", "full": true,
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &queryRecordingHandler{respBody: "[]"}
			d := &HTTPHandlerDispatcher{
				Handler: rec,
				UserResolver: fixedUserResolver(&models.User{
					ID: "user-1", Name: "Dave", Email: "dave@example.com",
				}),
			}
			ctx := WithDispatchInput(context.Background(), tc.input)
			if _, err := d.Dispatch(ctx, []string{"item", "history"}, nil); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			got := rec.gotQuery.Get("summary") == "true"
			if got != tc.wantSummary {
				t.Errorf("summary=%v, want %v (query %q)", got, tc.wantSummary, rec.gotQuery.Encode())
			}
		})
	}
}

// The limit has to survive the trip to the server too, not just reach the
// action — the action's injection is worthless if the dispatcher drops it.
func TestDispatchItemHistory_ForwardsLimitToTheEndpoint(t *testing.T) {
	rec := &queryRecordingHandler{respBody: "[]"}
	d := &HTTPHandlerDispatcher{
		Handler: rec,
		UserResolver: fixedUserResolver(&models.User{
			ID: "user-1", Name: "Dave", Email: "dave@example.com",
		}),
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "ref": "TASK-5", "limit": float64(12),
	})
	if _, err := d.Dispatch(ctx, []string{"item", "history"}, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := rec.gotQuery.Get("limit"); got != "12" {
		t.Errorf("limit reached the endpoint as %q, want \"12\" (query %q)",
			got, rec.gotQuery.Encode())
	}
}

// queryRecordingHandler captures the QUERY STRING, which the shared
// recordingHandler does not keep — these tests are entirely about what ends up
// in it.
type queryRecordingHandler struct {
	respBody string
	gotQuery interface {
		Get(string) string
		Encode() string
	}
}

func (h *queryRecordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.gotQuery = r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(h.respBody))
}

var _ http.Handler = (*queryRecordingHandler)(nil)
var _ = httptest.NewRecorder
