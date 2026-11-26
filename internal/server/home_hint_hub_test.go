package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/homehint"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

type homeHintRecorder struct {
	mu  sync.Mutex
	ch  <-chan watchevents.Notification
	got []watchevents.Notification
}

func recordHomeHints(t *testing.T, srv *Server) *homeHintRecorder {
	t.Helper()
	ch, unreg := srv.homeHints().register(4096)
	t.Cleanup(unreg)
	return &homeHintRecorder{ch: ch}
}

func (r *homeHintRecorder) drainLocked() {
	for {
		select {
		case n := <-r.ch:
			r.got = append(r.got, n)
		default:
			return
		}
	}
}

func (r *homeHintRecorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainLocked()
	return len(r.got)
}

func (r *homeHintRecorder) since(m int) []watchevents.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainLocked()
	if m >= len(r.got) {
		return nil
	}
	out := make([]watchevents.Notification, len(r.got)-m)
	copy(out, r.got[m:])
	return out
}

func TestHomeHint_BurstDoesNotEvictReplayRing(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := watchevents.NewWithReplaySize(8)
	srv.SetWatchEventsBus(bus)
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}

	for i := 0; i < 8; i++ {
		srv.publishWatchNotification(watchevents.Notification{
			WorkspaceID:  "ws",
			CollectionID: "c",
			ItemID:       fmt.Sprintf("item-%d", i),
			Kind:         watchevents.KindStatusChange,
		})
	}
	initial := bus.EventsSince(0)
	if len(initial) != 8 {
		t.Fatalf("initial replay len = %d, want 8", len(initial))
	}
	firstID := initial[0].ID

	for i := 0; i < 50; i++ {
		srv.publishHomeChanged("ws", "c", fmt.Sprintf("i%d", i))
	}

	// MemoryBus.EventsSince(firstID-1) returns nil on a full ring by design
	// (sinceID <= base and sinceID < oldestID when count == size). Checking
	// EventsSince(firstID) proves firstID was not evicted (even one hint on the
	// bus would evict firstID and make EventsSince(firstID) return nil), and
	// EventsSince(0) confirms all 8 status changes remain in the ring.
	afterFirst := bus.EventsSince(firstID)
	if afterFirst == nil {
		t.Fatalf("EventsSince(%d) returned nil; home_changed burst evicted firstID from the replay ring", firstID)
	}
	if len(afterFirst) != 7 {
		t.Fatalf("EventsSince(%d) len = %d, want 7", firstID, len(afterFirst))
	}
	got := bus.EventsSince(0)
	if got == nil {
		t.Fatal("EventsSince(0) returned nil")
	}
	if len(got) != 8 {
		t.Fatalf("EventsSince(0) len = %d, want 8", len(got))
	}
	if got[0].ID != firstID {
		t.Fatalf("got[0].ID = %d, want %d (firstID evicted)", got[0].ID, firstID)
	}
	for i, n := range got {
		if n.Kind != watchevents.KindStatusChange {
			t.Fatalf("event[%d].Kind = %q, want %q", i, n.Kind, watchevents.KindStatusChange)
		}
	}
}

func TestHomeHint_SlowHomeConsumerDropsHintsNotEvents(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	bus := watchevents.New()
	srv.SetWatchEventsBus(bus)
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}

	hintCh, unreg := srv.homeHints().register(homeHintStreamBuffer)
	defer unreg()

	mainCh, gaps, err := bus.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer bus.Unsubscribe(mainCh)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10*homeHintStreamBuffer; i++ {
			srv.publishHomeChanged("ws", "c", fmt.Sprintf("i%d", i))
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("emitting hints with an unread subscriber blocked past 1s")
	}

	if got := len(hintCh); got != homeHintStreamBuffer {
		t.Fatalf("len(hintCh) = %d, want %d", got, homeHintStreamBuffer)
	}
	if got := len(mainCh); got != 0 {
		t.Fatalf("len(mainCh) = %d, want 0", got)
	}
	select {
	case <-gaps:
		t.Fatal("slow home hint consumer triggered a watch bus gap")
	default:
	}

	srv.publishWatchNotification(watchevents.Notification{
		WorkspaceID:  "ws",
		CollectionID: "c",
		ItemID:       "status-item",
		Kind:         watchevents.KindStatusChange,
	})
	select {
	case n := <-mainCh:
		if n.Kind != watchevents.KindStatusChange || n.ItemID != "status-item" {
			t.Fatalf("got unexpected notification: %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("status change was not delivered on mainCh within 1s")
	}
}

func TestHomeHint_NonHomeStreamNotRegistered(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	user := mkAgentsUser(t, srv, "hh-reg-user@example.com", "HH Reg User")
	tok := mkAgentsPAT(t, srv, user.ID, "")

	noHomeCtx, cancelNoHome := context.WithCancel(context.Background())
	defer cancelNoHome()
	chNoHome := connectWatchStream(noHomeCtx, t, ts.URL, tok)
	waitForWatchEvent(t, chNoHome, 3*time.Second) // connected
	if got := srv.homeHints().count(); got != 0 {
		t.Fatalf("non-home stream registered on hub: count = %d, want 0", got)
	}

	homeCtx, cancelHome := context.WithCancel(context.Background())
	chHome := connectWatchStreamWithHeadersAndQuery(homeCtx, t, ts.URL, tok, nil, "home=true")
	waitForWatchEvent(t, chHome, 3*time.Second) // connected
	if got := srv.homeHints().count(); got != 1 {
		t.Fatalf("home=true stream hub count = %d, want 1", got)
	}

	cancelHome()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.homeHints().count() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("home=true stream did not unregister within 2s: count = %d", srv.homeHints().count())
}

func TestWatchStream_HomeHintFrameCarriesNoEventID(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner := mkAgentsUser(t, srv, "hh-noid-owner@example.com", "HH NoID Owner")
	ownerTok := mkAgentsPAT(t, srv, owner.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "NoID WS", Slug: "noid-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member owner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events/stream?home=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ownerTok)

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("connect stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	type sseBlock struct {
		lines []string
	}
	blocks := make(chan sseBlock, 16)
	go func() {
		defer close(blocks)
		sc := bufio.NewScanner(resp.Body)
		var cur []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(cur) > 0 {
					blocks <- sseBlock{lines: cur}
					cur = nil
				}
				continue
			}
			cur = append(cur, line)
		}
	}()

	// Wait for connected block.
	select {
	case blk, ok := <-blocks:
		if !ok {
			t.Fatal("stream closed before connected event")
		}
		joined := strings.Join(blk.lines, "\n")
		if !strings.Contains(joined, "event: connected") {
			t.Fatalf("first block was not connected: %q", joined)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for connected block")
	}

	srv.publishHomeChanged(ws.ID, "", "")

	deadline := time.After(3 * time.Second)
	for {
		select {
		case blk, ok := <-blocks:
			if !ok {
				t.Fatal("stream closed before home_changed block")
			}
			var dataLine string
			for _, line := range blk.lines {
				if strings.HasPrefix(line, "data: ") {
					dataLine = strings.TrimPrefix(line, "data: ")
				}
			}
			if dataLine == "" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(dataLine), &payload); err != nil {
				continue
			}
			if payload["kind"] != watchevents.KindHomeChanged {
				continue
			}
			for _, line := range blk.lines {
				if strings.HasPrefix(line, "id:") {
					t.Fatalf("home_changed SSE block carried an id line: %q (full block: %v)", line, blk.lines)
				}
			}
			if id, ok := payload["id"].(float64); !ok || id != 0 {
				t.Fatalf("home_changed payload id = %v, want 0 (payload: %+v)", payload["id"], payload)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for home_changed SSE block")
		}
	}
}

func TestHomeHint_CrossesInstancesOverRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := testServer(t), testServer(t)

	clientA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = clientA.Close() })
	busA := watchevents.NewRedisBus(clientA)
	t.Cleanup(func() { busA.Close() })
	a.SetWatchEventsBus(busA)
	a.SetHomeHintTransport(homehint.NewRedisTransport(clientA, redisns.Default))
	t.Cleanup(func() { a.SetHomeHintTransport(nil) })
	a.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}

	clientB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = clientB.Close() })
	busB := watchevents.NewRedisBus(clientB)
	t.Cleanup(func() { busB.Close() })
	b.SetWatchEventsBus(busB)
	b.SetHomeHintTransport(homehint.NewRedisTransport(clientB, redisns.Default))
	t.Cleanup(func() { b.SetHomeHintTransport(nil) })

	user := mkAgentsUser(t, b, "hh-redis@example.com", "HH Redis")
	tok := mkAgentsPAT(t, b, user.ID, "")
	wsB, err := b.store.CreateWorkspace(models.WorkspaceCreate{Name: "Redis WS", Slug: "redis-ws", OwnerID: user.ID})
	if err != nil {
		t.Fatalf("wsB: %v", err)
	}
	if err := b.store.AddWorkspaceMember(wsB.ID, user.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}

	tsB := httptest.NewServer(b)
	defer tsB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chB := connectWatchStreamWithHeadersAndQuery(ctx, t, tsB.URL, tok, nil, "home=true")
	waitForWatchEvent(t, chB, 3*time.Second) // connected

	a.publishHomeChanged(wsB.ID, "", "")

	got := waitForHomeHint(t, chB, 3*time.Second)
	if got["workspace"] != wsB.Slug {
		t.Fatalf("workspace = %v, want %q", got["workspace"], wsB.Slug)
	}
	for _, n := range busA.EventsSince(0) {
		if n.Kind == watchevents.KindHomeChanged {
			t.Fatalf("busA replay ring contains KindHomeChanged: %+v", n)
		}
	}
	for _, n := range busB.EventsSince(0) {
		if n.Kind == watchevents.KindHomeChanged {
			t.Fatalf("busB replay ring contains KindHomeChanged: %+v", n)
		}
	}
}

func TestHomeHint_OwnPublishDeliveredOnceWithTransport(t *testing.T) {
	mr := miniredis.RunT(t)
	srv := testServerWithWatchEvents(t)
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	srv.SetHomeHintTransport(homehint.NewRedisTransport(client, redisns.Default))
	t.Cleanup(func() { srv.SetHomeHintTransport(nil) })

	rec := recordHomeHints(t, srv)
	srv.publishHomeChanged("ws-self", "c1", "i1")
	time.Sleep(300 * time.Millisecond)

	got := rec.since(0)
	if len(got) != 1 {
		t.Fatalf("own publish delivered %d times, want 1: %+v", len(got), got)
	}
}

type blockingHomeHintTransport struct {
	release     chan struct{}
	cancelled   chan struct{}
	once        sync.Once
	entered     chan struct{}
	enteredOnce sync.Once
}

func (b *blockingHomeHintTransport) Publish(ctx context.Context, _ homehint.Message) error {
	b.enteredOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		b.once.Do(func() { close(b.cancelled) })
	}
	return nil
}

func (b *blockingHomeHintTransport) Subscribe(func(homehint.Message)) func() { return func() {} }

func TestHomeHint_StalledTransportDoesNotBlockWriter(t *testing.T) {
	srv := testServerWithWatchEvents(t)
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}
	bt := &blockingHomeHintTransport{
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
		entered:   make(chan struct{}),
	}
	srv.SetHomeHintTransport(bt)
	ch, unreg := srv.homeHints().register(homeHintStreamBuffer)
	defer unreg()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2*homeHintPublisherQueue; i++ {
			srv.publishHomeChanged("ws-stalled", "c1", fmt.Sprintf("i%d", i))
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%d hints against a stalled transport blocked the caller past 1s", 2*homeHintPublisherQueue)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("local registered reader did not receive a hint with a stalled transport")
	}

	select {
	case <-bt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the publisher worker never called Publish on a full queue")
	}
	srv.SetHomeHintTransport(nil)
	select {
	case <-bt.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("clearing the transport did not cancel the worker's blocked publish")
	}
}

type countingHomeHintTransport struct {
	mu                sync.Mutex
	subscribes, stops int
	handle            func(homehint.Message)
}

func (c *countingHomeHintTransport) Publish(_ context.Context, m homehint.Message) error {
	c.mu.Lock()
	h := c.handle
	c.mu.Unlock()
	if h != nil {
		h(m)
	}
	return nil
}

func (c *countingHomeHintTransport) Subscribe(h func(homehint.Message)) func() {
	c.mu.Lock()
	c.subscribes++
	c.handle = h
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.stops++
		c.handle = nil
		c.mu.Unlock()
	}
}

func TestHomeHint_TransportInstallIsIdempotent(t *testing.T) {
	srv := testServer(t)
	first, second := &countingHomeHintTransport{}, &countingHomeHintTransport{}
	srv.SetHomeHintTransport(first)
	srv.SetHomeHintTransport(first)
	if first.subscribes != 1 {
		t.Fatalf("same transport installed twice subscribed %d times", first.subscribes)
	}
	srv.SetHomeHintTransport(second)
	if first.stops != 1 || second.subscribes != 1 {
		t.Fatalf("replacement: first stops=%d, second subscribes=%d", first.stops, second.subscribes)
	}
	srv.SetHomeHintTransport(nil)
	if second.stops != 1 {
		t.Fatalf("nil did not stop the subscription (stops=%d)", second.stops)
	}
}
