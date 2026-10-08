package metrics

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collab"
)

// TASK-3501: the adapter maps each report to its own series, with counts that
// differ per series so a crossed wire cannot satisfy them.
func TestCollabObserverMapsEachReport(t *testing.T) {
	m := New()
	obs := NewCollabObserver(m)
	for i := 0; i < 7; i++ {
		obs.ResumeJoined()
	}
	obs.ResumeForceRefreshed(collab.ResumeRefreshPruned)
	for i := 0; i < 2; i++ {
		obs.ResumeForceRefreshed(collab.ResumeRefreshBehindMin)
	}
	for i := 0; i < 3; i++ {
		obs.ResumeForceRefreshed(collab.ResumeRefreshRestored)
	}

	assertCounter(t, m, "pad_collab_resumes_total", nil, 7)
	assertCounter(t, m, "pad_collab_resume_force_refreshes_total", map[string]string{"reason": collab.ResumeRefreshPruned}, 1)
	assertCounter(t, m, "pad_collab_resume_force_refreshes_total", map[string]string{"reason": collab.ResumeRefreshBehindMin}, 2)
	assertCounter(t, m, "pad_collab_resume_force_refreshes_total", map[string]string{"reason": collab.ResumeRefreshRestored}, 3)
}
