package metrics

import "github.com/PerpetualSoftware/pad/internal/collab"

// CollabObserver adapts a Metrics into a collab.Observer (TASK-3501).
type CollabObserver struct {
	m *Metrics
}

// NewCollabObserver returns an observer writing into m.
func NewCollabObserver(m *Metrics) *CollabObserver {
	return &CollabObserver{m: m}
}

var _ collab.Observer = (*CollabObserver)(nil)
var _ collab.OverflowObserver = (*CollabObserver)(nil)
var _ collab.CompactedResumeObserver = (*CollabObserver)(nil)

// CompactedResumeAdmitted counts a resume admitted into a snapshot (TASK-3531).
func (o *CollabObserver) CompactedResumeAdmitted() {
	o.m.CollabCompactedResumesTotal.Inc()
}

// OverflowClosed counts a peer closed for a dropped op (TASK-1273).
func (o *CollabObserver) OverflowClosed() {
	o.m.CollabOverflowClosesTotal.Inc()
}

// ResumeJoined is unlabelled: the item is the only dimension on offer, and it
// is unbounded.
func (o *CollabObserver) ResumeJoined() {
	o.m.CollabResumesTotal.Inc()
}

// ResumeForceRefreshed is labelled by reason, which the collab package bounds.
func (o *CollabObserver) ResumeForceRefreshed(reason string) {
	o.m.CollabResumeForceRefreshesTotal.WithLabelValues(reason).Inc()
}
