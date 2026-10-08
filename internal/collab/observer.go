package collab

import "sync"

// Observer receives counts the room manager can see on the collab Join path
// (TASK-3501). It exists to measure how often an editor tab loses its socket
// and comes back, before deciding IDEA-3494 (persisting unsent offline edits).
//
// WHAT THESE COUNT, AND WHAT THEY DO NOT. A RESUME is a Join that announces a
// cursor (`?since=N`, N > 0): a tab whose editor had been anchored to the
// op-log reconnecting on the same page load. It is a PROXY for "this tab was
// offline", and a biased one:
//   - inflated by anything that drops a socket without the user being offline:
//     laptop sleep, a network blip, a server restart or deploy;
//   - blind to a tab that went offline and was then CLOSED or crashed, which
//     never comes back to be counted. That is the case IDEA-3494 is about, and
//     only the browser can see it;
//   - blind to whether the tab had made edits while away: the relay does not
//     parse Yjs, so the catch-up frame looks the same either way.
//
// A force_refresh on a resume is an UPPER BOUND on hand-backs
// (OfflineRecoveryNotice): the client shows one only when it also held unsent
// edits, which the server cannot see.
type Observer interface {
	// ResumeJoined counts one Join with a cursor (since > 0), once per Join
	// whatever its outcome.
	ResumeJoined()
	// ResumeForceRefreshed counts a resume refused with force_refresh, by
	// reason (one of the ResumeRefresh constants, bounded for a label).
	ResumeForceRefreshed(reason string)
}

// Reasons a resume is refused with force_refresh.
const (
	// ResumeRefreshPruned: the item's op-log is empty, so the whole log was
	// pruned (dormant GC, PruneAndApply, a schema rebuild).
	ResumeRefreshPruned = "pruned"
	// ResumeRefreshBehindMin: the cursor is below the op-log's MIN, so rows the
	// client still needed were pruned.
	ResumeRefreshBehindMin = "behind_min"
	// ResumeRefreshRestored: the client's seed predates a version restore.
	ResumeRefreshRestored = "restored"
)

// observable is the nil-safe Observer holder the RoomManager embeds.
// Reporting before SetObserver is a no-op.
type observable struct {
	obsMu sync.RWMutex
	obs   Observer
}

// SetObserver attaches an Observer; nil detaches. Safe while running.
func (o *observable) SetObserver(obs Observer) {
	o.obsMu.Lock()
	o.obs = obs
	o.obsMu.Unlock()
}

func (o *observable) observer() Observer {
	o.obsMu.RLock()
	defer o.obsMu.RUnlock()
	return o.obs
}

func (o *observable) reportResumeJoined() {
	if obs := o.observer(); obs != nil {
		obs.ResumeJoined()
	}
}

func (o *observable) reportResumeForceRefreshed(reason string) {
	if obs := o.observer(); obs != nil {
		obs.ResumeForceRefreshed(reason)
	}
}
