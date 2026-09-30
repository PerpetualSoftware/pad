package materialize

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

func putVarUint(b []byte, n uint64) []byte {
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return append(b, byte(n))
}

func putVarString(b []byte, s string) []byte {
	b = putVarUint(b, uint64(len(s)))
	return append(b, s...)
}

// deepNestFrame builds one y-protocols Update frame whose Yjs update (v1
// encoding) nests depth XmlElements named node, each the only child of the
// one before, under the 'default' fragment the editor binds. The update is
// tiny — about 12 bytes per level — and what it costs to materialize is not:
// every level is a recursion in the Yjs → ProseMirror conversion and in the
// markdown serializer, and nested blockquotes repeat their "> " prefix on
// every line, so the output is quadratic in depth.
func deepNestFrame(depth int, node string) []byte {
	const client = 7
	var u []byte
	u = putVarUint(u, 1) // one client's structs
	u = putVarUint(u, uint64(depth+3))
	u = putVarUint(u, client)
	u = putVarUint(u, 0) // first clock
	parent := func(i int) {
		if i == 0 {
			u = putVarUint(u, 1) // parent is a root type, named:
			u = putVarString(u, "default")
			return
		}
		u = putVarUint(u, 0) // parent is the item (client, i-1)
		u = putVarUint(u, client)
		u = putVarUint(u, uint64(i-1))
	}
	element := func(i int, name string) {
		u = append(u, 7) // info: ContentType, no origins, no parentSub
		parent(i)
		u = putVarUint(u, 3) // YXmlElement
		u = putVarString(u, name)
	}
	for i := 0; i < depth; i++ {
		element(i, node)
	}
	// Innermost: a paragraph holding the text "x", so the tree is valid
	// content all the way down and nothing is dropped as empty.
	element(depth, "paragraph")
	u = append(u, 7) // ContentType
	parent(depth + 1)
	u = putVarUint(u, 6) // YXmlText
	u = append(u, 4)     // info: ContentString
	parent(depth + 2)
	u = putVarString(u, "x")
	u = putVarUint(u, 0) // empty delete set
	f := []byte{0, 2}    // MESSAGE_SYNC, messageYjsUpdate
	f = putVarUint(f, uint64(len(u)))
	return append(f, u...)
}

// The generator is well-formed: a shallow nest materializes to what the
// editor makes of it.
func TestDeepNestFrameShallow(t *testing.T) {
	r := runner(t)
	md, err := r.Materialize(context.Background(), Job{Rows: [][]byte{deepNestFrame(3, "blockquote")}, SchemaVersion: r.SchemaVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if md != "> > > x" {
		t.Fatalf("3 nested blockquotes: %q, want %q", md, "> > > x")
	}
}

// TestSupervisorAdversarialDeepNesting sends the spike's shape — a ~1 MB
// deep-nesting update — to a REAL capped worker. In-process this is the input
// that drove Go past 5 GB into its fatal out-of-memory. Here the worker must
// fail the job (killed on its cap, or on the deadline), the parent must be
// untouched, and the next job must succeed on a fresh worker.
func TestSupervisorAdversarialDeepNesting(t *testing.T) {
	if raceEnabled {
		skipCapTest(t, "the race runtime cannot start under an address-space cap")
	}
	if testing.Short() {
		t.Skip("spawns a real worker and drives it to its cap")
	}
	depth := 60_000 // a 1,063,536-byte frame: the spike's ~1 MB
	if v := os.Getenv("PAD_TEST_NEST_DEPTH"); v != "" {
		depth, _ = strconv.Atoi(v)
	}
	frame := deepNestFrame(depth, "blockquote")
	h := newHarness(t, "worker", func(c *SupervisorConfig) {
		c.MemLimit = 2 << 30
		c.Timeout = 30 * time.Second
		c.capOverride = nil
	})
	r := runner(t)
	start := time.Now()
	md, err := h.s.Materialize(context.Background(), Job{Rows: [][]byte{frame}, SchemaVersion: r.SchemaVersion()})
	t.Logf("depth %d, frame %d bytes: %d bytes of markdown, err %v, after %s", depth, len(frame), len(md), err, time.Since(start).Round(time.Millisecond))
	if !errors.Is(err, ErrMemoryLimit) && !errors.Is(err, ErrDeadline) && !errors.Is(err, ErrChildDied) {
		t.Fatalf("got %d bytes, %v; want the worker to fail the job", len(md), err)
	}
	for _, l := range h.log.find("materialize worker stopped") {
		t.Log(l)
	}
	ok := loadCorpus(t)[0]
	got, err := h.s.Materialize(context.Background(), ok.job(t, r.SchemaVersion()))
	if err != nil || got != ok.Expected {
		t.Fatalf("job after the adversarial one: %q, %v", got, err)
	}
}
