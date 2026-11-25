package server

import "github.com/PerpetualSoftware/pad/internal/watchevents"

// publishHomeChanged publishes a home_changed refetch hint onto the watch
// event bus (opt-in via ?home=true) and increments pad_home_hints_total.
// Call only after the underlying write has committed.
func (s *Server) publishHomeChanged(workspaceID, collectionID, itemID string) {
	if workspaceID == "" || s.watchEvents == nil {
		return
	}
	if s.metrics != nil {
		s.metrics.HomeHintsTotal.Inc()
	}
	s.publishWatchNotification(watchevents.Notification{
		WorkspaceID:  workspaceID,
		CollectionID: collectionID,
		ItemID:       itemID,
		Kind:         watchevents.KindHomeChanged,
	})
}
