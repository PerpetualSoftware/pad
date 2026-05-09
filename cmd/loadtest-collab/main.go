// Command loadtest-collab simulates N concurrent WebSocket clients
// editing the same Y.Doc-backed item, measures broadcast fanout
// latency and op-log growth, and prints a summary.
//
// This is a deliberately minimal y-websocket client — it speaks just
// enough of the protocol (BinaryMessage frames with the
// yMessageSync=0 first-byte discriminator) to exercise the dumb-
// relay's persist + broadcast path. It does NOT integrate with
// y-protocols/sync proper; the binary payloads are synthetic
// "update" stubs that the server persists to op-log and rebroadcasts
// to peers without parsing. That's enough surface to load-test the
// transport / fan-out / op-log layers without dragging a Yjs Go
// port into the binary.
//
// Usage:
//
//	go run ./cmd/loadtest-collab \
//	    -url ws://localhost:7777/api/v1/collab/<itemID> \
//	    -cookie "<session-cookie>" \
//	    -clients 25 \
//	    -duration 30s \
//	    -ops-per-sec 2
//
// The session cookie is required when the server has been bootstrapped
// (any normal install). Grab it from your browser DevTools (Cookies →
// `pad_session`) or `cat ~/.pad/credentials.json | jq -r .token` and
// pass as `pad_session=<value>`.
//
// The output is a one-shot summary on stdout:
//
//	clients=25 duration=30s ops_per_sec_per_client=2
//	  total_ops_sent=1483 total_ops_received=37075 broadcast_fanout=24.99x
//	  latency_p50=2ms p95=8ms p99=21ms
//	  errors=0
//
// Per TASK-1270 (PLAN-1248).
package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// yMessageSync is the first-byte discriminator for sync frames in
	// the y-websocket protocol — must match
	// internal/collab/room.go::yMessageSync.
	yMessageSync byte = 0
	// schemaVersion is the value the server validates against on
	// upgrade (TASK-1268). Must match
	// web/src/lib/collab/schemaVersion.ts::SCHEMA_VERSION.
	schemaVersion = "1"
)

func main() {
	url := flag.String("url", "", "WebSocket URL, e.g. ws://localhost:7777/api/v1/collab/<itemID>")
	cookie := flag.String("cookie", "", "Cookie header to send (e.g. \"pad_session=abc123\"). Pass either -cookie OR -token (or both — they set independent headers and don't conflict).")
	token := flag.String("token", "", "Bearer token for Authorization header. Pass either -token OR -cookie when the server has been bootstrapped; both can be set together.")
	clients := flag.Int("clients", 5, "Number of concurrent simulated editors")
	dur := flag.Duration("duration", 30*time.Second, "How long to run the test")
	opsPerSec := flag.Float64("ops-per-sec", 2.0, "Per-client op send rate (Hz)")
	frameBytes := flag.Int("frame-bytes", 32, "Size in bytes of the synthetic update payload (excluding the 1-byte sync header)")
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "loadtest-collab: -url is required")
		flag.Usage()
		os.Exit(2)
	}
	if *clients < 1 {
		fmt.Fprintln(os.Stderr, "loadtest-collab: -clients must be >= 1")
		os.Exit(2)
	}
	if *opsPerSec <= 0 {
		fmt.Fprintln(os.Stderr, "loadtest-collab: -ops-per-sec must be > 0")
		os.Exit(2)
	}

	// Append the schema_version query param so the v1 handshake
	// validation accepts us. URL-fragile but tests should only ever
	// hit the documented endpoint shape.
	dialURL := *url
	if strings.Contains(dialURL, "?") {
		dialURL += "&schema_version=" + schemaVersion
	} else {
		dialURL += "?schema_version=" + schemaVersion
	}

	hdr := http.Header{}
	if *cookie != "" {
		hdr.Set("Cookie", *cookie)
	}
	if *token != "" {
		hdr.Set("Authorization", "Bearer "+*token)
	}

	ctx := newRunContext(*dur)

	var wg sync.WaitGroup
	for i := 0; i < *clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runClient(ctx, id, dialURL, hdr, *opsPerSec, *frameBytes)
		}(i)
	}

	// Stop after duration; clients exit on the closed `done` channel.
	time.Sleep(*dur)
	close(ctx.done)
	wg.Wait()

	ctx.printSummary(*clients, *dur, *opsPerSec)
}

// runContext is the load-test's shared state: a stop signal, op
// counters, and a latency histogram. Lives on the stack of main()
// and is shared across every client goroutine — atomic counters keep
// us from needing a mutex on the hot send/recv path.
type runContext struct {
	done chan struct{}

	// startedAt is captured at construction. readSendTimestamp uses
	// it as a cutoff so op-log replay frames from PRIOR test runs
	// (whose embedded timestamps predate this run) don't inflate
	// the latency percentiles. Without this, every fresh connect's
	// replay would record ages-old "latencies" and the p95/p99
	// numbers would be junk. Per Codex review [P2].
	startedAt time.Time

	totalSent     atomic.Int64
	totalReceived atomic.Int64
	totalErrors   atomic.Int64

	// latencyMu guards the latency slice. Each client appends its
	// observed round-trip latencies (frame send → first peer recv).
	// We compute percentiles at the end. Pre-allocated to a generous
	// cap to avoid per-append GC churn during the run.
	latencyMu sync.Mutex
	latencies []time.Duration
}

func newRunContext(d time.Duration) *runContext {
	return &runContext{
		done:      make(chan struct{}),
		startedAt: time.Now(),
		latencies: make([]time.Duration, 0, 100_000),
	}
}

func (rc *runContext) recordLatency(d time.Duration) {
	rc.latencyMu.Lock()
	rc.latencies = append(rc.latencies, d)
	rc.latencyMu.Unlock()
}

// runClient opens one WebSocket, fires synthetic updates at the
// configured rate, and consumes inbound frames from peers. Exits
// when `done` closes or the connection errors. Each outbound frame
// is tagged with the originating client's id and a millisecond
// timestamp so the receive side can compute round-trip latency.
func runClient(rc *runContext, id int, dialURL string, hdr http.Header, opsPerSec float64, frameBytes int) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(dialURL, hdr)
	if err != nil {
		rc.totalErrors.Add(1)
		log.Printf("client %d: dial: %v", id, err)
		return
	}
	defer conn.Close()

	// Reader goroutine: every frame received counts toward
	// totalReceived; if the frame was tagged by another client AND
	// its embedded timestamp falls within this run's window, we
	// record the latency. Frames whose embedded timestamp predates
	// `rc.startedAt` are op-log replay rows from a prior session
	// and would inflate percentiles if recorded. Per Codex review
	// [P2].
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			rc.totalReceived.Add(1)
			if t := readSendTimestamp(data); !t.IsZero() && !t.Before(rc.startedAt) {
				rc.recordLatency(time.Since(t))
			}
		}
	}()

	// Watchdog: when shutdown is signalled, close the conn from
	// outside the writer's loop. The writer can be blocked inside
	// `conn.WriteMessage` under server backpressure; if the only
	// path to `conn.Close()` were the writer's own select-case,
	// `wg.Wait()` would hang forever in that scenario. Closing the
	// conn from a watchdog unblocks both the writer (next send
	// errors) and the reader (ReadMessage returns), so the
	// goroutines can drain. Per Codex review [P2].
	go func() {
		<-rc.done
		_ = conn.Close()
	}()

	// Write deadline per send acts as a second line of defence: if
	// the watchdog hasn't fired but the server is backpressuring
	// individual frames, the writer wakes up periodically rather
	// than wedging indefinitely. 5s is generous enough that healthy
	// servers never trip it but tight enough that a stuck send
	// fails fast.
	const writeDeadline = 5 * time.Second

	period := time.Duration(float64(time.Second) / opsPerSec)
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-rc.done:
			// Watchdog above already closed the conn; just wait
			// for the reader to drain and exit.
			<-readerDone
			return
		case <-ticker.C:
			frame, err := buildFrame(id, frameBytes)
			if err != nil {
				log.Printf("client %d: build frame: %v", id, err)
				rc.totalErrors.Add(1)
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				// If shutdown is signalled, the watchdog above
				// closed the conn from under us — this write error
				// is expected, not a real failure. Don't pollute
				// the errors counter with it. Per Codex review
				// round 2 NIT.
				if isClosedDone(rc.done) {
					return
				}
				rc.totalErrors.Add(1)
				return
			}
			rc.totalSent.Add(1)
		}
	}
}

// isClosedDone is a non-blocking check for "has rc.done been
// closed?". Used by the writer's error path to distinguish a
// real network error from a watchdog-induced close during shutdown.
func isClosedDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// buildFrame builds a synthetic sync frame. Layout (offsets into the
// returned []byte, NOT into the user-facing frameBytes parameter):
//
//	[0]:       yMessageSync (0x00) — the relay's first-byte discriminator
//	[1..9]:    little-endian unix-nano timestamp (used for latency)
//	[9..17]:   little-endian client id (informational)
//	[17..]:    random padding so the total length is `frameBytes+1`
//
// `frameBytes` is the documented "payload size excluding the 1-byte
// sync header". The 16-byte tagged metadata occupies bytes [1..17] of
// the returned buffer, so a `frameBytes < 16` request gets bumped to
// 16. The buffer length is `frameBytes+1` (header byte + payload).
//
// The server's relay is type-0/type-1 byte-discriminator only — it
// doesn't parse the rest. Real Yjs updates would be y-protocol
// VarUint structures here; we use a tagged random blob because
// load-testing the relay's persist+broadcast path doesn't require
// real Y.Doc semantics.
//
// Returns an error rather than killing the process via log.Fatalf
// when crypto/rand fails — a test goroutine that exits via Fatalf
// skips the wg.Done deferred in the caller and would also drop the
// summary print. Per Codex review NIT.
func buildFrame(clientID, frameBytes int) ([]byte, error) {
	if frameBytes < 16 {
		frameBytes = 16 // minimum to fit the tagged metadata
	}
	buf := make([]byte, frameBytes+1)
	buf[0] = yMessageSync
	binary.LittleEndian.PutUint64(buf[1:9], uint64(time.Now().UnixNano()))
	binary.LittleEndian.PutUint64(buf[9:17], uint64(clientID))
	if frameBytes > 16 {
		if _, err := rand.Read(buf[17:]); err != nil {
			return nil, fmt.Errorf("rand.Read: %w", err)
		}
	}
	return buf, nil
}

// readSendTimestamp extracts the embedded ts from a frame produced
// by buildFrame. Returns zero time if the frame is too short or its
// header byte doesn't look like ours (ignores op-log replay frames
// from prior runs that may flow on connect).
func readSendTimestamp(data []byte) time.Time {
	if len(data) < 17 || data[0] != yMessageSync {
		return time.Time{}
	}
	ns := binary.LittleEndian.Uint64(data[1:9])
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns))
}

func (rc *runContext) printSummary(clients int, dur time.Duration, opsPerSec float64) {
	totalSent := rc.totalSent.Load()
	totalReceived := rc.totalReceived.Load()
	totalErrors := rc.totalErrors.Load()

	rc.latencyMu.Lock()
	lats := append([]time.Duration(nil), rc.latencies...)
	rc.latencyMu.Unlock()

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })

	p := func(percentile float64) time.Duration {
		if len(lats) == 0 {
			return 0
		}
		idx := int(percentile * float64(len(lats)-1))
		return lats[idx]
	}

	// Expected fan-out: every send by client A is broadcast to the
	// other (clients-1) peers, so totalReceived ≈ totalSent *
	// (clients-1) on a clean run. Reporting the actual ratio
	// surfaces dropped frames or relay backpressure.
	fanout := 0.0
	if totalSent > 0 {
		fanout = float64(totalReceived) / float64(totalSent)
	}

	fmt.Println()
	fmt.Printf("clients=%d duration=%s ops_per_sec_per_client=%.2f\n", clients, dur, opsPerSec)
	fmt.Printf("  total_ops_sent=%d total_ops_received=%d broadcast_fanout=%.2fx (expected ~%dx)\n",
		totalSent, totalReceived, fanout, clients-1)
	fmt.Printf("  latency: n=%d p50=%v p95=%v p99=%v max=%v\n",
		len(lats), p(0.50), p(0.95), p(0.99),
		func() time.Duration {
			if len(lats) == 0 {
				return 0
			}
			return lats[len(lats)-1]
		}())
	fmt.Printf("  errors=%d\n", totalErrors)
}
