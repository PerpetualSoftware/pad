package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The content-routed update orderings classify store.UpdateItem's typed refusals
// through writeTypedItemRefusal alone. Without a schedule_active arm, a body change
// together with a close on an item with a live recurring schedule answered 500 there
// while the plain path answered 409.
func TestWriteTypedItemRefusalIncludesScheduleActive(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	item := &models.Item{ID: "item-1", Ref: "TASK-1", Slug: "task-1"}
	next := "2099-01-01T09:00:00Z"
	err := fmt.Errorf("update item: %w", &scheduleActiveError{details: scheduleActiveDetails{ScheduleID: "s1", NextAt: &next}})

	rec := httptest.NewRecorder()
	if !srv.writeTypedItemRefusal(rec, item, err, false) {
		t.Fatal("schedule_active must be a typed refusal on the content-routed orderings")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				ScheduleID string  `json:"schedule_id"`
				NextAt     *string `json:"next_at"`
			} `json:"details"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), jerr)
	}
	if body.Error.Code != "schedule_active" || body.Error.Details.ScheduleID != "s1" || body.Error.Details.NextAt == nil || *body.Error.Details.NextAt != next {
		t.Fatalf("unexpected envelope: %s", rec.Body.String())
	}
}
