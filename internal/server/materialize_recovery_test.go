package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-2198 U4: the op-log recovery triggers, worker and failure budget, and
// the end-to-end recovery through the real materializer bundle.

// ---------------------------------------------------------------- fixtures

// fakeMaterializer answers every job with fn and counts calls.
type fakeMaterializer struct {
	calls atomic.Int64
	mu    sync.Mutex
	jobs  []materialize.Job
	fn    func(job materialize.Job) (string, error)
}

func (f *fakeMaterializer) Materialize(_ context.Context, job materialize.Job) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.mu.Unlock()
	if f.fn == nil {
		return "recovered body", nil
	}
	return f.fn(job)
}

// recoveryRunner is ONE in-process Runner over the embedded bundle; loading
// costs about a second. It satisfies Materializer, so the server drives the
// real JavaScript without a child process.
var (
	recoveryRunnerOnce sync.Once
	recoveryRunnerVal  *materialize.Runner
	recoveryRunnerErr  error
)

func recoveryRunner(t *testing.T) *materialize.Runner {
	t.Helper()
	recoveryRunnerOnce.Do(func() { recoveryRunnerVal, recoveryRunnerErr = materialize.New(pad.MaterializerJS) })
	if recoveryRunnerErr != nil {
		t.Fatalf("load the materializer bundle: %v", recoveryRunnerErr)
	}
	return recoveryRunnerVal
}

type recoveryFixture struct {
	srv  *Server
	rm   *collab.RoomManager
	r    *materializeRecovery
	ws   *models.Workspace
	coll *models.Collection

	mu        sync.Mutex
	processed []string
	results   map[string]materializeResult
	done      chan string
}

// newRecoveryFixture: a test server with a room manager (grace TTL grace) and
// an op-log recovery worker over m, NOT started. cfg zero fields default.
func newRecoveryFixture(t *testing.T, m Materializer, grace time.Duration, cfg materializeRecoveryConfig) *recoveryFixture {
	t.Helper()
	srv := testServer(t)
	bus := collab.NewMemoryOpBus()
	t.Cleanup(bus.Close)
	rm := collab.NewRoomManagerWithConfig(srv.store, bus, collab.RoomManagerConfig{GraceTTL: grace})
	t.Cleanup(rm.Close)
	srv.SetCollabRoomManager(rm)
	f := &recoveryFixture{srv: srv, rm: rm, results: map[string]materializeResult{}, done: make(chan string, 256)}
	if m != nil {
		f.r = newMaterializeRecovery(srv, m, cfg)
		f.r.processed = func(id string, res materializeResult) {
			f.mu.Lock()
			f.processed = append(f.processed, id)
			f.results[id] = res
			f.mu.Unlock()
			f.done <- id
		}
		srv.materializeRecovery = f.r
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Recovery WS", Slug: "ws"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Notes", Prefix: "NOTE", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	f.ws, f.coll = ws, coll
	return f
}

func (f *recoveryFixture) item(t *testing.T, title, content string) *models.Item {
	t.Helper()
	it, err := f.srv.store.CreateItem(f.ws.ID, f.coll.ID, models.ItemCreate{Title: title, Content: content, Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return it
}

func (f *recoveryFixture) appendRows(t *testing.T, itemID string, rows ...[]byte) int64 {
	t.Helper()
	var last int64
	for _, r := range rows {
		id, err := f.srv.store.AppendYjsUpdate(itemID, r, collab.DefaultSchemaVersion)
		if err != nil {
			t.Fatalf("AppendYjsUpdate: %v", err)
		}
		last = id
	}
	return last
}

func (f *recoveryFixture) waitProcessed(t *testing.T, itemID string, within time.Duration) materializeResult {
	t.Helper()
	deadline := time.After(within)
	for {
		f.mu.Lock()
		res, ok := f.results[itemID]
		f.mu.Unlock()
		if ok {
			return res
		}
		select {
		case <-f.done:
		case <-deadline:
			t.Fatalf("item %s was not processed within %s", itemID, within)
		}
	}
}

func (f *recoveryFixture) queueSnapshot() []string {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	return append([]string(nil), f.r.queue...)
}

// contentFrame is a content-bearing Update frame, distinct per n.
func contentFrame(n byte) []byte { return []byte{0x00, 0x02, 0x05, 0x01, n, 0x00, 0x7F, 0x00} }

func dialAndClose(t *testing.T, baseURL, itemID string) {
	t.Helper()
	c, resp, err := dialCollab(t, baseURL, itemID, nil, "")
	if err != nil {
		st := ""
		if resp != nil {
			st = resp.Status
		}
		t.Fatalf("dial %s: %v %s", itemID, err, st)
	}
	// Read until the op_log_cursor text frame: the join has registered.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		mt, _, rerr := c.ReadMessage()
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
		if mt == 1 { // websocket.TextMessage
			break
		}
	}
	c.Close()
}

// ---------------------------------------------------------------- T3

// T3's decision, unit level: a reclaimed room queues its item only when the
// item still has content-bearing rows above its watermark.
func TestMaterializeRoomIdleEnqueuesOnlyWithPendingRows(t *testing.T) {
	f := newRecoveryFixture(t, &fakeMaterializer{}, time.Minute, materializeRecoveryConfig{})
	pending := f.item(t, "Pending", "stale")
	f.appendRows(t, pending.ID, contentFrame(1))
	flushed := f.item(t, "Flushed", "fine")
	c := f.appendRows(t, flushed.ID, contentFrame(2))
	if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(flushed.ID, c); err != nil {
		t.Fatal(err)
	}
	empty := f.item(t, "No op-log", "fine")

	f.r.onRoomIdle(pending.ID)
	f.r.onRoomIdle(flushed.ID)
	f.r.onRoomIdle(empty.ID)
	q := f.queueSnapshot()
	if len(q) != 1 || q[0] != pending.ID {
		t.Fatalf("queue after room idle = %v, want only %s", q, pending.ID)
	}
	// De-duplicated.
	f.r.onRoomIdle(pending.ID)
	if q := f.queueSnapshot(); len(q) != 1 {
		t.Fatalf("queue after a second idle = %v, want one entry", q)
	}
}

// T3 end to end: a real collab room is reclaimed at the end of its grace TTL,
// the hook installed by StartMaterializeRecovery fires, and the pending item
// is recovered while the clean one is never handed to the materializer.
func TestMaterializeRoomIdleRecoversThroughTheHook(t *testing.T) {
	fake := &fakeMaterializer{}
	f := newRecoveryFixture(t, fake, 50*time.Millisecond, materializeRecoveryConfig{sweepInterval: time.Hour})
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)

	pending := f.item(t, "Pending", "stale")
	f.appendRows(t, pending.ID, contentFrame(1))
	clean := f.item(t, "Clean", "fine")

	f.srv.StartMaterializeRecovery()
	dialAndClose(t, ts.URL, clean.ID)
	dialAndClose(t, ts.URL, pending.ID)

	if res := f.waitProcessed(t, pending.ID, 5*time.Second); res != mrApplied {
		t.Fatalf("pending item result %q, want applied", res)
	}
	got, _ := f.srv.store.GetItem(pending.ID)
	if got.Content != "recovered body" || got.ContentState != "" {
		t.Fatalf("after room idle: content %q state %q", got.Content, got.ContentState)
	}
	// The clean item's room is reclaimed too (both dials closed before the
	// pending one was processed); it must not have been queued.
	deadline := time.Now().Add(2 * time.Second)
	for f.rm.RoomCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.results[clean.ID]; ok {
		t.Fatalf("the clean item was queued on room idle")
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("materializer calls = %d, want 1", n)
	}
}

// ---------------------------------------------------------------- T1

func TestMaterializeSweepQueuesDormantPendingItemsWithoutARoom(t *testing.T) {
	f := newRecoveryFixture(t, &fakeMaterializer{}, time.Minute, materializeRecoveryConfig{
		// The sweep's clock is an hour ahead, so rows written now are dormant.
		now: func() time.Time { return time.Now().Add(time.Hour) },
	})
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)

	dormant := f.item(t, "Dormant", "stale")
	f.appendRows(t, dormant.ID, contentFrame(1))
	withRoom := f.item(t, "Open room", "stale")
	f.appendRows(t, withRoom.ID, contentFrame(2))
	flushed := f.item(t, "Flushed", "fine")
	c := f.appendRows(t, flushed.ID, contentFrame(3))
	if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(flushed.ID, c); err != nil {
		t.Fatal(err)
	}

	// Hold a room open on withRoom (the connection stays up).
	conn, _, err := dialCollab(t, ts.URL, withRoom.ID, nil, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for !f.rm.HasRoom(withRoom.ID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !f.rm.HasRoom(withRoom.ID) {
		t.Fatal("no room for the open connection")
	}

	f.r.sweep()
	q := f.queueSnapshot()
	if len(q) != 1 || q[0] != dormant.ID {
		t.Fatalf("sweep queued %v, want only the dormant item %s", q, dormant.ID)
	}

	// The worker re-checks: even if an item with a room is queued, it is not
	// materialized.
	if res := f.r.process(withRoom.ID); res != mrRoomOpen {
		t.Fatalf("process(item with a room) = %q, want room_open", res)
	}
}

// ---------------------------------------------------------------- budget

func TestMaterializeFailureBudgetStopsAfterKAndResumesOnNewRows(t *testing.T) {
	clock := time.Now()
	var clockMu sync.Mutex
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	fake := &fakeMaterializer{fn: func(materialize.Job) (string, error) {
		return "", fmt.Errorf("%w: exit status 2", materialize.ErrChildDied)
	}}
	f := newRecoveryFixture(t, fake, time.Minute, materializeRecoveryConfig{now: now, maxFailures: 3, backoffBase: time.Minute})
	it := f.item(t, "Poison", "stale")
	f.appendRows(t, it.ID, contentFrame(1))

	want := []struct {
		res   materializeResult
		calls int64
		after time.Duration
	}{
		{mrFailed, 1, 0},
		{mrBackoff, 1, 0},                     // inside the 1m backoff
		{mrFailed, 2, time.Minute + 1},        // backoff over
		{mrBackoff, 2, time.Minute},           // 2m backoff now
		{mrFailed, 3, time.Minute + 1},        // K reached
		{mrBudgetExceeded, 3, 24 * time.Hour}, // no time heals it
	}
	for i, w := range want {
		advance(w.after)
		if res := f.r.process(it.ID); res != w.res || fake.calls.Load() != w.calls {
			t.Fatalf("step %d: result %q calls %d, want %q calls %d", i, res, fake.calls.Load(), w.res, w.calls)
		}
	}

	// A new op-log row resets the budget.
	f.appendRows(t, it.ID, contentFrame(2))
	if res := f.r.process(it.ID); res != mrFailed || fake.calls.Load() != 4 {
		t.Fatalf("after a new row: result %q calls %d, want failed, 4", res, fake.calls.Load())
	}
	// And a success clears it.
	fake.fn = nil
	advance(time.Hour)
	if res := f.r.process(it.ID); res != mrApplied {
		t.Fatalf("after recovery of the input: %q, want applied", res)
	}
	f.r.mu.Lock()
	_, left := f.r.budget[it.ID]
	f.r.mu.Unlock()
	if left {
		t.Fatal("budget entry survived a success")
	}
}

// Rows written under another editor schema are skipped, not materialized.
func TestMaterializeSkipsForeignSchemaVersion(t *testing.T) {
	fake := &fakeMaterializer{}
	f := newRecoveryFixture(t, fake, time.Minute, materializeRecoveryConfig{})
	it := f.item(t, "Old schema", "stale")
	if _, err := f.srv.store.AppendYjsUpdate(it.ID, contentFrame(1), "0-old"); err != nil {
		t.Fatal(err)
	}
	if res := f.r.process(it.ID); res != mrSchemaVersion || fake.calls.Load() != 0 {
		t.Fatalf("process = %q calls %d, want schema_version, 0", res, fake.calls.Load())
	}
	if res := f.r.process(it.ID); res != mrBudgetExceeded {
		t.Fatalf("second pass = %q, want budget_exhausted until the op-log changes", res)
	}
}

// ---------------------------------------------------------------- disabled

// With no Materializer (PAD_MATERIALIZE=off leaves it nil) nothing is
// installed: a room going idle over a pending item leaves it pending.
func TestMaterializeRecoveryDisabledInstallsNothing(t *testing.T) {
	f := newRecoveryFixture(t, nil, 30*time.Millisecond, materializeRecoveryConfig{})
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	f.srv.SetMaterializer(nil)
	f.srv.StartMaterializeRecovery()
	if f.srv.materializeRecovery != nil {
		t.Fatal("a recovery worker exists with no materializer")
	}
	it := f.item(t, "Pending", "stale")
	f.appendRows(t, it.ID, contentFrame(1))
	dialAndClose(t, ts.URL, it.ID)
	deadline := time.Now().Add(2 * time.Second)
	for f.rm.RoomCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got, _ := f.srv.store.GetItem(it.ID)
	if got.Content != "stale" || got.ContentState != models.ContentStatePendingFlush {
		t.Fatalf("disabled: content %q state %q, want untouched and pending", got.Content, got.ContentState)
	}
}

// ---------------------------------------------------------------- end to end

type corpusCaseE2E struct {
	Name          string                  `json:"name"`
	Rows          []string                `json:"rows"`
	LinkIndex     []materialize.LinkEntry `json:"link_index"`
	WorkspaceSlug string                  `json:"workspace_slug"`
	Expected      string                  `json:"expected"`
}

func decodeRows(t *testing.T, rows []string) [][]byte {
	t.Helper()
	out := make([][]byte, len(rows))
	for i, s := range rows {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

// TestMaterializeRecoveryEndToEnd: real y-protocols op-logs from the U2
// corpus (recorded from two live Editor.svelte mounts) are inserted as items'
// op-logs over a stale items.content; the REAL sweep loop, worker and bundle
// recover them. The stored body must be the live editor's flush output, the
// item must read clean on GET, and a writer still holding the pre-recovery
// seq must be refused rather than overwrite the recovered text.
//
// Only corpus cases recorded with an EMPTY link index are used: the server
// builds its own index from the workspace, and none of these documents link
// to an item that exists here, so the flush pipeline's link conversion
// matches nothing, exactly as with the empty index the expected output was
// recorded with.
func TestMaterializeRecoveryEndToEnd(t *testing.T) {
	raw, err := os.ReadFile("../materialize/testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []corpusCaseE2E
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}

	f := newRecoveryFixture(t, recoveryRunner(t), time.Minute, materializeRecoveryConfig{
		sweepInterval: 20 * time.Millisecond,
		now:           func() time.Time { return time.Now().Add(time.Hour) },
	})

	type seeded struct {
		c    corpusCaseE2E
		item *models.Item
	}
	var cases []seeded
	for _, c := range corpus {
		// A cross-workspace link (/-/r/...) is rewritten to [[ws::REF]]
		// whenever the index is non-empty, whatever it holds, and a real
		// workspace's index always holds at least the item itself; the
		// corpus's empty-index cases were recorded with the conversion
		// skipped. Cases containing one are left out rather than papered over.
		if len(c.LinkIndex) != 0 || strings.Contains(c.Expected, "](/-/r/") {
			continue
		}
		if c.WorkspaceSlug != f.ws.Slug {
			t.Fatalf("%s: corpus workspace %q, fixture %q", c.Name, c.WorkspaceSlug, f.ws.Slug)
		}
		it := f.item(t, "E2E "+c.Name, "stale body before recovery")
		f.appendRows(t, it.ID, decodeRows(t, c.Rows)...)
		cases = append(cases, seeded{c, it})
	}
	if len(cases) < 4 {
		t.Fatalf("only %d empty-index corpus cases; the instrument needs more", len(cases))
	}

	// Pre-recovery state, and the refusal a token-guarded write gets while
	// the rows are pending (BUG-3133), for contrast with the one after.
	first := cases[0].item
	pre, _ := f.srv.store.GetItem(first.ID)
	if pre.ContentState != models.ContentStatePendingFlush {
		t.Fatalf("precondition: content_state %q", pre.ContentState)
	}
	preSeq := pre.Seq

	f.srv.StartMaterializeRecovery()
	for _, c := range cases {
		if res := f.waitProcessed(t, c.item.ID, 60*time.Second); res != mrApplied {
			t.Fatalf("%s: result %q, want applied", c.c.Name, res)
		}
	}

	for _, c := range cases {
		rr := doRequest(f.srv, http.MethodGet, "/api/v1/workspaces/ws/items/"+c.item.Slug, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: GET %d %s", c.c.Name, rr.Code, rr.Body.String())
		}
		var got models.Item
		parseJSON(t, rr, &got)
		if got.Content != c.c.Expected {
			t.Errorf("%s: recovered body differs from the live editor's flush\n--- got ---\n%s\n--- want ---\n%s", c.c.Name, got.Content, c.c.Expected)
		}
		if got.ContentState == models.ContentStatePendingFlush {
			t.Errorf("%s: content_state still %q after recovery", c.c.Name, got.ContentState)
		}
	}

	body := map[string]any{"content": "a caller's body from before the recovery", "expected_seq": preSeq}
	rr := doRequest(f.srv, http.MethodPatch, "/api/v1/workspaces/ws/items/"+first.Slug, body)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "update_conflict") {
		t.Fatalf("stale expected_seq PATCH = %d %s, want 409 update_conflict", rr.Code, rr.Body.String())
	}
	after, _ := f.srv.store.GetItem(first.ID)
	if after.Content != cases[0].c.Expected {
		t.Fatalf("the refused PATCH changed the body")
	}
	t.Logf("%d corpus op-logs recovered end to end", len(cases))
}

// The version history shows the recovery as the system's, on the wire.
func TestMaterializeRecoveryVersionOnTheWire(t *testing.T) {
	f := newRecoveryFixture(t, &fakeMaterializer{}, time.Minute, materializeRecoveryConfig{})
	it := f.item(t, "Versioned", "stale")
	f.appendRows(t, it.ID, contentFrame(1))
	if res := f.r.process(it.ID); res != mrApplied {
		t.Fatalf("process = %q", res)
	}
	rr := doRequest(f.srv, http.MethodGet, "/api/v1/workspaces/ws/items/"+it.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("versions: %d %s", rr.Code, rr.Body.String())
	}
	var vs []models.Version
	parseJSON(t, rr, &vs)
	if len(vs) == 0 || vs[0].CreatedBy != models.VersionCreatedBySystem || vs[0].Source != models.VersionSourceRecovery ||
		vs[0].ChangeSummary != models.RecoveryChangeSummary {
		t.Fatalf("newest version = %+v", vs)
	}
}

// A client cannot claim the recovery label for its own write.
func TestClientCannotClaimRecoveryVersionSource(t *testing.T) {
	f := newRecoveryFixture(t, nil, time.Minute, materializeRecoveryConfig{})
	it := f.item(t, "Spoof", "before")
	rr := doRequest(f.srv, http.MethodPatch, "/api/v1/workspaces/ws/items/"+it.Slug,
		map[string]any{"content": "after", "version_source": models.VersionSourceRecovery})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH %d %s", rr.Code, rr.Body.String())
	}
	vs, err := f.srv.store.ListItemVersions(it.ID)
	if err != nil || len(vs) == 0 {
		t.Fatalf("versions: %v %v", vs, err)
	}
	if vs[0].Source == models.VersionSourceRecovery {
		t.Fatal("a client write was labelled as a recovery")
	}
}

// ErrNoMemoryCap (U3: no cap could be established, so the supervisor refused
// the job and warned once itself) counts against the budget like any failure,
// but is not logged per item. The control is ErrChildDied, which is.
func TestMaterializeNoMemoryCapCountsSilently(t *testing.T) {
	for _, tc := range []struct {
		err      error
		wantWarn bool
	}{
		{fmt.Errorf("%w: no cap mechanism on plan9", materialize.ErrNoMemoryCap), false},
		{fmt.Errorf("%w: exit status 2", materialize.ErrChildDied), true},
	} {
		var buf bytes.Buffer
		clock := time.Now()
		fake := &fakeMaterializer{fn: func(materialize.Job) (string, error) { return "", tc.err }}
		f := newRecoveryFixture(t, fake, time.Minute, materializeRecoveryConfig{
			maxFailures: 3, backoffBase: time.Minute,
			now:    func() time.Time { return clock },
			logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		})
		it := f.item(t, "Uncapped", "stale")
		f.appendRows(t, it.ID, contentFrame(1))
		for i := 0; i < 3; i++ {
			if res := f.r.process(it.ID); res != mrFailed {
				t.Fatalf("%v: pass %d = %q, want failed", tc.err, i, res)
			}
			clock = clock.Add(time.Hour)
		}
		if res := f.r.process(it.ID); res != mrBudgetExceeded || fake.calls.Load() != 3 {
			t.Fatalf("%v: after K failures %q with %d calls, want budget_exhausted, 3", tc.err, res, fake.calls.Load())
		}
		if got := strings.Contains(buf.String(), "level=WARN"); got != tc.wantWarn {
			t.Fatalf("%v: per-item WARN logged = %v, want %v\n%s", tc.err, got, tc.wantWarn, buf.String())
		}
	}
}

// An item that uses up its budget leaves exactly ONE line naming it, its
// failure count and the last error kind; a budget reset (op-log growth) and a
// second exhaustion leave exactly one more.
func TestMaterializeExhaustionLogsOncePerExhaustion(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Now()
	fake := &fakeMaterializer{fn: func(materialize.Job) (string, error) {
		return "", fmt.Errorf("%w: exit status 2", materialize.ErrChildDied)
	}}
	f := newRecoveryFixture(t, fake, time.Minute, materializeRecoveryConfig{
		maxFailures: 3, backoffBase: time.Minute,
		now:    func() time.Time { return clock },
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	it := f.item(t, "Stuck", "stale")
	f.appendRows(t, it.ID, contentFrame(1))
	const msg = "exhausted its failure budget"
	exhaustLines := func() []string {
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, msg) {
				out = append(out, l)
			}
		}
		return out
	}
	drive := func(passes int) {
		for i := 0; i < passes; i++ {
			f.r.process(it.ID)
			clock = clock.Add(time.Hour)
		}
	}

	drive(3 + 5) // K failures, then five refused passes
	lines := exhaustLines()
	if len(lines) != 1 {
		t.Fatalf("exhaustion lines after one exhaustion = %d, want 1\n%s", len(lines), buf.String())
	}
	for _, want := range []string{"level=WARN", "item_id=" + it.ID, "failures=3", "last_error_kind=worker_died"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("exhaustion line lacks %q: %s", want, lines[0])
		}
	}

	f.appendRows(t, it.ID, contentFrame(2)) // op-log grows: the budget resets
	drive(3 + 5)
	if n := len(exhaustLines()); n != 2 {
		t.Fatalf("exhaustion lines after a reset and a second exhaustion = %d, want 2\n%s", n, buf.String())
	}
	if fake.calls.Load() != 6 {
		t.Fatalf("materializer calls %d, want 6", fake.calls.Load())
	}

	// The schema-version skip is an exhaustion too, logged once.
	buf.Reset()
	old := f.item(t, "Old schema", "stale")
	if _, err := f.srv.store.AppendYjsUpdate(old.ID, contentFrame(3), "0-old"); err != nil {
		t.Fatal(err)
	}
	f.r.process(old.ID)
	f.r.process(old.ID)
	if l := exhaustLines(); len(l) != 1 || !strings.Contains(l[0], "last_error_kind=schema_version") {
		t.Fatalf("schema-version exhaustion lines = %v", l)
	}
}

// Items skipped for set-aside rows are counted, reported on the sweep's one
// summary line, and never logged per item.
func TestMaterializeSetAsideSkipsAreCountedNotLogged(t *testing.T) {
	var buf bytes.Buffer
	fake := &fakeMaterializer{}
	f := newRecoveryFixture(t, fake, time.Minute, materializeRecoveryConfig{
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	it := f.item(t, "Aside", "stale")
	f.appendRows(t, it.ID, contentFrame(1))
	if _, _, err := f.srv.store.SetAsideAndClearOpLog(it.ID); err != nil {
		t.Fatal(err)
	}
	f.appendRows(t, it.ID, contentFrame(2)) // pending again, over set-aside rows

	f.r.onRoomIdle(it.ID) // the room trigger does queue it: its op-log is pending
	if q := f.queueSnapshot(); len(q) != 1 {
		t.Fatalf("queue = %v", q)
	}
	for i := 0; i < 3; i++ {
		if res := f.r.process(it.ID); res != mrSetAside {
			t.Fatalf("process = %q, want set_aside", res)
		}
	}
	if n := f.r.setAsideSkipped.Load(); n != 3 {
		t.Fatalf("set-aside skip count = %d, want 3", n)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("a set-aside item reached the materializer")
	}
	if strings.Contains(buf.String(), it.ID) {
		t.Fatalf("set-aside skips logged per item:\n%s", buf.String())
	}

	f.r.sweep()
	f.r.sweep() // no news: no second line
	var sweepLines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "op-log recovery sweep") {
			sweepLines = append(sweepLines, l)
		}
	}
	if len(sweepLines) != 1 || !strings.Contains(sweepLines[0], "set_aside_skipped_total=3") {
		t.Fatalf("sweep summary lines = %v, want one carrying set_aside_skipped_total=3", sweepLines)
	}
}

// STARVATION (codex r1): the sweep reads a bounded page, and the room check
// and the failure budget filter it afterwards. With materializeSweepLimit+1
// dormant pending items of which the first materializeSweepLimit fail every
// time, the last one must still be recovered, within
// ceil(N / limit) + 1 = 3 sweeps, at the REAL page size.
func TestMaterializeSweepReachesEveryCandidate(t *testing.T) {
	clock := time.Now().Add(time.Hour) // rows written now are dormant
	var targetFrame []byte
	fake := &fakeMaterializer{fn: func(job materialize.Job) (string, error) {
		if len(job.Rows) == 1 && bytes.Equal(job.Rows[0], targetFrame) {
			return "recovered body", nil
		}
		return "", fmt.Errorf("%w: exit status 2", materialize.ErrChildDied)
	}}
	f := newRecoveryFixture(t, fake, time.Minute, materializeRecoveryConfig{
		now:    func() time.Time { return clock },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	n := materializeSweepLimit + 1
	frameOf := map[string][]byte{}
	for i := 0; i < n; i++ {
		it := f.item(t, fmt.Sprintf("Backlog %03d", i), "stale")
		fr := []byte{0x00, 0x02, 0x05, 0x01, byte(i >> 8), byte(i), 0x7F, 0x00}
		f.appendRows(t, it.ID, fr)
		frameOf[it.ID] = fr
	}
	all, err := f.srv.store.ListMaterializeCandidates(clock.Add(-materializeDormancy), store.MaterializeCursor{}, 10*n)
	if err != nil || len(all) != n {
		t.Fatalf("candidates %d (%v), want %d", len(all), err, n)
	}
	target := all[n-1].ItemID // the one every page read from the start leaves out
	targetFrame = frameOf[target]

	for tick := 1; tick <= 3; tick++ {
		f.r.sweep()
		for {
			id, ok := f.r.dequeue()
			if !ok {
				break
			}
			if res := f.r.process(id); id == target && res == mrApplied {
				t.Logf("the %dth candidate was recovered on sweep %d", n, tick)
				return
			}
		}
		clock = clock.Add(time.Hour) // every backoff expires between ticks
	}
	t.Fatalf("the %dth candidate was never recovered in 3 sweeps", n)
}
