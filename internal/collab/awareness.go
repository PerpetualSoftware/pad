package collab

import (
	"errors"
	"sync"
)

// Awareness removal on disconnect (TASK-2206, audit C88).
//
// The room relays awareness (presence: carets, selections, names) without
// persisting it. Before this, it also kept no record of whose presence a
// connection carried, so a tab that went away without saying so (a hard
// close, a dropped network) left its caret in every peer's document until
// y-protocols' 30-second timeout reaped it. y-websocket's server answers that
// by remembering the client IDs each connection announced and, when the
// connection closes, broadcasting an awareness update that removes them. This
// file is that, with one difference that matters here.
//
// FIRST SENDER OWNS. Our wsProvider (before TASK-2206, and any tab still
// running that code until it reloads) re-sends EVERY awareness change it
// applies, peers' entries included. A connection's frames therefore carry
// other users' client IDs, so "every ID a connection sent is that
// connection's" would remove live peers when one tab closes. A client's own
// frames reach the server before any peer could have received them to echo,
// so the first live connection to send a client ID is the one that owns it,
// and an echo never transfers ownership.
//
// A NEWER CLOCK MOVES IT. The server records each frame before relaying it,
// so an echo can carry at most the clock already recorded, never a newer one:
// a newer clock comes from the client itself. When it arrives on another
// connection, the client has reconnected while its old connection is still
// closing, and ownership moves with it. Without that, the old connection's
// close would remove a client that is live on the new one (codex r1). The
// provider advances its clock on every (re)connect, so this always applies.
//
// Accepted, not defended: a connection can claim another client's ID first
// and, on closing, remove that caret from peers' views until its owner's next
// heartbeat. Awareness is cosmetic and never document state, so this is not
// an integrity or confidentiality question (lead ruling, no auth work).

// maxOwnedPerConn bounds the IDs one connection can own. A real tab owns one
// (its Y.Doc's client ID); the cap only stops a misbehaving client growing
// the map without limit.
const maxOwnedPerConn = 64

// awarenessEntry is one client's entry in a y-protocols awareness update.
type awarenessEntry struct {
	clientID uint64
	clock    uint64
	// live is false when the state is JSON null: the client said it is gone.
	live bool
}

var errAwarenessFrame = errors.New("malformed awareness frame")

// maxSafeInteger is the largest integer y-protocols can write: client IDs and
// clocks are JavaScript numbers. A larger value is not a real client's, and a
// clock at the top of uint64 would wrap the removal's clock + 1 to 0 (codex r1).
const maxSafeInteger = 1<<53 - 1

// readVarUint reads a lib0 variable-length unsigned integer (7 bits per byte,
// high bit = more). At most 8 bytes (56 bits): y-protocols writes nothing above
// 2^53, and a longer encoding could carry bits a uint64 would silently drop
// (codex r4).
func readVarUint(b []byte, i int) (uint64, int, error) {
	var v uint64
	for shift := uint(0); shift < 56; shift += 7 {
		if i >= len(b) {
			return 0, i, errAwarenessFrame
		}
		c := b[i]
		i++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, i, nil
		}
	}
	return 0, i, errAwarenessFrame
}

func appendVarUint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// decodeAwarenessFrame parses a whole awareness WebSocket frame: the message
// type (1), then a length-prefixed update holding a count and, per entry, a
// client ID, a clock and a length-prefixed JSON state.
func decodeAwarenessFrame(frame []byte) ([]awarenessEntry, error) {
	typ, i, err := readVarUint(frame, 0)
	if err != nil || typ != yMessageAwareness {
		return nil, errAwarenessFrame
	}
	n, i, err := readVarUint(frame, i)
	if err != nil || uint64(len(frame)-i) < n {
		return nil, errAwarenessFrame
	}
	update := frame[i : i+int(n)]
	count, j, err := readVarUint(update, 0)
	if err != nil {
		return nil, errAwarenessFrame
	}
	if count > uint64(len(update)) { // every entry is at least 3 bytes
		return nil, errAwarenessFrame
	}
	out := make([]awarenessEntry, 0, count)
	for k := uint64(0); k < count; k++ {
		var e awarenessEntry
		if e.clientID, j, err = readVarUint(update, j); err != nil {
			return nil, err
		}
		if e.clock, j, err = readVarUint(update, j); err != nil {
			return nil, err
		}
		if e.clientID > maxSafeInteger || e.clock > maxSafeInteger {
			return nil, errAwarenessFrame
		}
		var sl uint64
		if sl, j, err = readVarUint(update, j); err != nil || uint64(len(update)-j) < sl {
			return nil, errAwarenessFrame
		}
		state := update[j : j+int(sl)]
		j += int(sl)
		e.live = string(state) != "null"
		out = append(out, e)
	}
	return out, nil
}

// encodeAwarenessRemoval builds the frame that tells peers each client is
// gone: its state null, one clock past the last one seen, which is what
// y-protocols' removeAwarenessStates sends.
func encodeAwarenessRemoval(entries []awarenessEntry) []byte {
	var update []byte
	update = appendVarUint(update, uint64(len(entries)))
	for _, e := range entries {
		update = appendVarUint(update, e.clientID)
		update = appendVarUint(update, e.clock+1)
		update = appendVarUint(update, 4)
		update = append(update, "null"...)
	}
	frame := appendVarUint(nil, yMessageAwareness)
	frame = appendVarUint(frame, uint64(len(update)))
	return append(frame, update...)
}

// awarenessTracker records, per room, which connection owns which client IDs
// and the last clock and liveness seen for each.
type awarenessTracker struct {
	mu    sync.Mutex
	owner map[uint64]uint64                    // client ID -> owning conn id
	owned map[uint64]map[uint64]awarenessEntry // conn id -> client ID -> last entry
}

func newAwarenessTracker() *awarenessTracker {
	return &awarenessTracker{owner: map[uint64]uint64{}, owned: map[uint64]map[uint64]awarenessEntry{}}
}

// observe records a frame a connection sent. A frame that does not decode is
// not tracked (it is still relayed by the caller).
func (t *awarenessTracker) observe(connID uint64, frame []byte) {
	entries, err := decodeAwarenessFrame(frame)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range entries {
		mine := t.owned[connID]
		if _, held := mine[e.clientID]; !held && len(mine) >= maxOwnedPerConn {
			// At the cap: take nothing, and so move nothing away from its
			// current owner, whose close must still remove it (codex r5).
			continue
		}
		if o, ok := t.owner[e.clientID]; ok && o != connID {
			prev := t.owned[o][e.clientID]
			if e.clock <= prev.clock {
				continue // an echo of another connection's client
			}
			// The client itself, reconnected on this connection.
			delete(t.owned[o], e.clientID)
			delete(t.owner, e.clientID)
		}
		if mine == nil {
			mine = map[uint64]awarenessEntry{}
			t.owned[connID] = mine
		}
		t.owner[e.clientID] = connID
		if prev, ok := mine[e.clientID]; ok && prev.clock > e.clock {
			continue // an older update overtaken in flight
		}
		mine[e.clientID] = e
	}
}

// release forgets a closing connection and returns the entries it owned that
// were still live: those the room must tell peers are gone.
func (t *awarenessTracker) release(connID uint64) []awarenessEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var gone []awarenessEntry
	for id, e := range t.owned[connID] {
		if t.owner[id] == connID {
			delete(t.owner, id)
		}
		if e.live {
			gone = append(gone, e)
		}
	}
	delete(t.owned, connID)
	return gone
}
