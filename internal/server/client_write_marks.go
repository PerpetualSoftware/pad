package server

import (
	"sync"
	"time"
)

// clientWriteMarks orders ONE browser tab's content writes to an item
// (BUG-3080).
//
// The item pane fires content PATCHes it never waits on — the teardown flushes
// in particular are `keepalive` requests sent from a page that is going away —
// and each carries the WHOLE body, so whichever reaches the store last wins. A
// tab that survives a cancelled unload, types, and is then really closed can
// have its older PATCH land after its newer one and silently put the old text
// back. No version token can order the two: both are dispatched before the tab
// sees either result, so both carry the same one, and the older landing first
// would get the NEWER refused. The tab can order them only by saying so, which
// is what `client_write` is: a per-page-load random id and a counter that only
// rises. This type remembers, per (item, tab), the highest counter a write was
// APPLIED at, and a lower one is refused — 409 `superseded_write`, not written.
//
// The check, the write and the mark are one step per key: `acquire` hands back
// the key's entry LOCKED, and the handler holds it until the write is done.
// Without that, the older request could pass its check, the newer pass and
// write, and the older then write last — the defect, moved inside the server.
// The lock is per (item, tab), so it only ever serialises one tab's own writes
// to one item against each other.
//
// BEST-EFFORT, and it degrades to the behaviour before it existed — never to
// anything worse. It is in memory and per instance:
//
//   - forgotten on restart, and not shared between instances on a
//     multi-instance deployment: a mark that is missing refuses nothing;
//   - BOUNDED at clientWriteMarkMaxEntries keys, each forgotten once idle for
//     clientWriteMarkMaxAge. The window it exists for is the seconds between
//     two requests of one tab; ten minutes is far past any keepalive request's
//     life, and the cap is far past the number of items a live instance's tabs
//     write to in that time. When the cap is hit, idle entries go first, oldest
//     first; an entry some request holds is never evicted;
//   - a write with no `client_write` (the CLI, MCP, any other API caller) is
//     never compared against a mark, and never makes one.
type clientWriteMarks struct {
	mu      sync.Mutex
	entries map[clientWriteKey]*clientWriteEntry
	now     func() time.Time
}

const (
	clientWriteMarkMaxEntries = 10000
	clientWriteMarkMaxAge     = 10 * time.Minute
)

type clientWriteKey struct {
	item string
	tab  string
}

type clientWriteEntry struct {
	// Held from the check to the end of the write; see the type comment.
	mu sync.Mutex
	// Guarded by clientWriteMarks.mu.
	applied int64
	touched time.Time
	holders int
}

func newClientWriteMarks() *clientWriteMarks {
	return &clientWriteMarks{entries: map[clientWriteKey]*clientWriteEntry{}, now: time.Now}
}

// acquire returns the entry for (item, tab) with its write lock HELD, and the
// highest counter applied for it so far (0 when there is none). The caller must
// call release exactly once.
func (m *clientWriteMarks) acquire(item, tab string) (*clientWriteEntry, int64) {
	key := clientWriteKey{item: item, tab: tab}
	m.mu.Lock()
	e := m.entries[key]
	if e == nil {
		m.evictLocked()
		e = &clientWriteEntry{}
		m.entries[key] = e
	}
	e.holders++
	e.touched = m.now()
	m.mu.Unlock()

	e.mu.Lock()
	m.mu.Lock()
	applied := e.applied
	m.mu.Unlock()
	return e, applied
}

// release records `n` as applied when the write went through, and drops the
// entry's write lock.
func (m *clientWriteMarks) release(e *clientWriteEntry, n int64, applied bool) {
	m.mu.Lock()
	if applied && n > e.applied {
		e.applied = n
	}
	e.holders--
	e.touched = m.now()
	m.mu.Unlock()
	e.mu.Unlock()
}

// evictLocked makes room for one more key. Idle entries past the age go
// first; if the map is still full, the oldest idle ones do. Called with m.mu
// held.
func (m *clientWriteMarks) evictLocked() {
	if len(m.entries) < clientWriteMarkMaxEntries {
		return
	}
	cutoff := m.now().Add(-clientWriteMarkMaxAge)
	for k, e := range m.entries {
		if e.holders == 0 && e.touched.Before(cutoff) {
			delete(m.entries, k)
		}
	}
	for len(m.entries) >= clientWriteMarkMaxEntries {
		var oldestKey clientWriteKey
		var oldest *clientWriteEntry
		for k, e := range m.entries {
			if e.holders == 0 && (oldest == nil || e.touched.Before(oldest.touched)) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			// Every entry is held by a request in flight. Grow past the cap
			// rather than drop a held mark; the next insert tries again.
			return
		}
		delete(m.entries, oldestKey)
	}
}
