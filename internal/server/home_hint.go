package server

import (
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

const defaultHomeHintCoalesceWindow = 500 * time.Millisecond

type homeHintTimer interface {
	Stop() bool
}

// homeHintCoalesceConfig is a TEST-ONLY seam (nil in production) for
// controlling publishHomeChanged's coalescing window and timer implementation.
type homeHintCoalesceConfig struct {
	window    time.Duration
	disabled  bool
	afterFunc func(d time.Duration, f func()) homeHintTimer
}

type homeHintEntry struct {
	workspaceID  string
	collectionID string
	itemID       string
	pending      bool
	gen          uint64
	timer        homeHintTimer
}

func defaultHomeHintAfterFunc(d time.Duration, f func()) homeHintTimer {
	return time.AfterFunc(d, f)
}

// publishHomeChanged publishes a home_changed refetch hint onto the watch
// event bus (opt-in via ?home=true) and increments pad_home_hints_total.
// Call only after the underlying write has committed.
//
// Hints are coalesced per (workspaceID, collectionID, itemID) key: the first
// hint for an idle key publishes immediately and opens a 500 ms window; any
// further hints for that key during the window are suppressed and collapsed
// into a single trailing-edge hint when the window closes (which itself opens
// a new window). Entries are deleted when a window ends with nothing pending,
// so the map stays bounded.
func (s *Server) publishHomeChanged(workspaceID, collectionID, itemID string) {
	if workspaceID == "" || s.watchEvents == nil {
		return
	}

	cfg := s.homeHintCoalesceForTesting
	if cfg != nil && cfg.disabled {
		s.emitHomeChanged(workspaceID, collectionID, itemID)
		return
	}

	window := defaultHomeHintCoalesceWindow
	afterFunc := defaultHomeHintAfterFunc
	if cfg != nil {
		if cfg.window > 0 {
			window = cfg.window
		}
		if cfg.afterFunc != nil {
			afterFunc = cfg.afterFunc
		}
	}

	key := workspaceID + "|" + collectionID + "|" + itemID

	s.homeHintMu.Lock()
	if s.homeHintStopped {
		s.homeHintMu.Unlock()
		return
	}
	if s.homeHintEntries == nil {
		s.homeHintEntries = make(map[string]*homeHintEntry)
	}
	if entry, exists := s.homeHintEntries[key]; exists {
		entry.pending = true
		s.homeHintMu.Unlock()
		return
	}
	entry := &homeHintEntry{
		workspaceID:  workspaceID,
		collectionID: collectionID,
		itemID:       itemID,
		gen:          1,
	}
	s.homeHintEntries[key] = entry
	s.homeHintMu.Unlock()

	s.emitHomeChanged(workspaceID, collectionID, itemID)

	s.homeHintMu.Lock()
	if s.homeHintStopped || s.homeHintEntries[key] != entry {
		if s.homeHintEntries[key] == entry {
			delete(s.homeHintEntries, key)
		}
		s.homeHintMu.Unlock()
		return
	}
	gen := entry.gen
	entry.timer = afterFunc(window, func() {
		s.onHomeHintWindowEnd(key, entry, gen, window, afterFunc)
	})
	s.homeHintMu.Unlock()
}

func (s *Server) onHomeHintWindowEnd(key string, entry *homeHintEntry, gen uint64, window time.Duration, afterFunc func(time.Duration, func()) homeHintTimer) {
	s.homeHintMu.Lock()
	if s.homeHintStopped || s.homeHintEntries[key] != entry || entry.gen != gen {
		s.homeHintMu.Unlock()
		return
	}
	if !entry.pending {
		delete(s.homeHintEntries, key)
		s.homeHintMu.Unlock()
		return
	}
	entry.pending = false
	entry.gen++
	nextGen := entry.gen
	s.homeHintWG.Add(1)
	s.homeHintMu.Unlock()

	defer s.homeHintWG.Done()
	s.emitHomeChanged(entry.workspaceID, entry.collectionID, entry.itemID)

	s.homeHintMu.Lock()
	if s.homeHintStopped || s.homeHintEntries[key] != entry || entry.gen != nextGen {
		if s.homeHintEntries[key] == entry {
			delete(s.homeHintEntries, key)
		}
		s.homeHintMu.Unlock()
		return
	}
	entry.timer = afterFunc(window, func() {
		s.onHomeHintWindowEnd(key, entry, nextGen, window, afterFunc)
	})
	s.homeHintMu.Unlock()
}

// stopHomeHintCoalescer cancels all pending coalescing timers and waits for
// any in-flight trailing-edge publish to return so timers never outlive Stop.
func (s *Server) stopHomeHintCoalescer() {
	s.homeHintMu.Lock()
	s.homeHintStopped = true
	for key, entry := range s.homeHintEntries {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		delete(s.homeHintEntries, key)
	}
	s.homeHintMu.Unlock()
	s.homeHintWG.Wait()
}

func (s *Server) emitHomeChanged(workspaceID, collectionID, itemID string) {
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
