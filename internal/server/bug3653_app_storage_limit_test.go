package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3653 part B: an app upload past the plan's storage cap answers 403
// storage_limit_reached, with no totals (DOC-3371 §4).
func TestBUG3653_AppStorageLimitIs403WithoutTotals(t *testing.T) {
	rr := httptest.NewRecorder()
	writeAppStoreError(rr, store.ErrAppStorageLimit)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `"storage_limit_reached"`) {
		t.Fatalf("response = %d %s, want 403 storage_limit_reached", rr.Code, rr.Body.String())
	}
	if strings.ContainsAny(rr.Body.String(), "0123456789") {
		t.Errorf("the refusal carries a number: %s", rr.Body.String())
	}
}
