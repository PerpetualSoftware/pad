package attachments

import "sync"

// InFlight counts uploads currently materializing a blob, per content hash.
// An upload marks its hash between the blob landing on disk and its row
// committing; the orphan GC's rowless-blob sweep skips a marked hash, so it
// cannot reclaim a blob whose row is about to exist.
//
// One InFlight is shared by every upload path in a process: the human
// upload, thumbnailing, transforms, bundle import and the app upload
// (TASK-3396), so they all fence against the same sweep. It is per process;
// cross-instance coordination between uploads and GC is not provided here.
//
// Increment, map-store, decrement and delete all run under one mutex, so a
// concurrent Active call cannot observe a stale 0 between the last release
// and the next mark (Codex P1 on PR #307 round 2, which this type was moved
// out of Server verbatim to preserve).
type InFlight struct {
	mu     sync.Mutex
	hashes map[string]int64
}

// Mark increments the count for hash and returns the release, which the
// caller MUST call exactly once (defer it).
func (f *InFlight) Mark(hash string) func() {
	f.Hold(hash)
	return func() { f.Release(hash) }
}

// Hold and Release are Mark without the closure, for a caller that may not
// call through a function value (the app store's boundary test refuses one).
// Every Hold needs exactly one Release.
func (f *InFlight) Hold(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hashes == nil {
		f.hashes = make(map[string]int64)
	}
	f.hashes[hash]++
}

// Release undoes one Hold.
func (f *InFlight) Release(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashes[hash]--
	if f.hashes[hash] <= 0 {
		delete(f.hashes, hash)
	}
}

// Lock and Unlock expose the mutex to a caller that must hold it across its
// own check-and-act: the orphan GC and workspace purge keep it held from
// reading a hash's count through deleting that hash's blob, so no upload can
// mark the hash in between. Read the count with CountLocked while holding it.
func (f *InFlight) Lock()   { f.mu.Lock() }
func (f *InFlight) Unlock() { f.mu.Unlock() }

// CountLocked is the number of uploads holding hash. The caller holds Lock.
func (f *InFlight) CountLocked(hash string) int64 { return f.hashes[hash] }

// Active reports whether any upload holds hash.
func (f *InFlight) Active(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hashes[hash] > 0
}
