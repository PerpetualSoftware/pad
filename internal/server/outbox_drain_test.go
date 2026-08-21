package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// Tests for the outbox drain (TASK-2714). They call runOutboxDrainTick
// directly rather than starting the loop: the loop's ticker is not the
// behaviour under test, and racing a free-running goroutine would make every
// assertion here a timing question.

type capturedDelivery struct {
	Event   string          `json:"event"`
	ID      string          `json:"id"`
	BatchID string          `json:"batch_id"`
	Data    json.RawMessage `json:"data"`
}

type webhookSink struct {
	mu       sync.Mutex
	got      []capturedDelivery
	status   int
	server   *httptest.Server
	requests int
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	sink := &webhookSink{status: http.StatusOK}
	sink.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var d capturedDelivery
		_ = json.Unmarshal(body, &d)
		sink.mu.Lock()
		sink.got = append(sink.got, d)
		sink.requests++
		status := sink.status
		sink.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(sink.server.Close)
	return sink
}

func (s *webhookSink) deliveries() []capturedDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedDelivery, len(s.got))
	copy(out, s.got)
	return out
}

func (s *webhookSink) setStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

// drainFixture wires a server to a webhook sink and returns the workspace the
// events belong to.
func drainFixture(t *testing.T) (*Server, *webhookSink, *models.Workspace) {
	t.Helper()
	srv := testServer(t)
	sink := newWebhookSink(t)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Drain", Slug: "drain"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := srv.store.CreateWebhook(ws.ID, models.WebhookCreate{URL: sink.server.URL, Events: `["*"]`}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	d := webhooks.NewDispatcher(srv.store)
	d.SkipSSRF = true
	srv.SetWebhookDispatcher(d)
	return srv, sink, ws
}

func pendingOutboxCount(t *testing.T, srv *Server) int {
	t.Helper()
	evs, err := srv.store.ListPendingOutboxEvents(1000)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	return len(evs)
}

// TestOutboxDrain_DeliversAndAcksASingleEvent is the base case: a stored event
// reaches the wire once, carrying its row id as the dedupe key, and stops
// being pending.
func TestOutboxDrain_DeliversAndAcksASingleEvent(t *testing.T) {
	srv, sink, ws := drainFixture(t)
	col := createDrainCollection(t, srv, ws.ID)
	item := createDrainItem(t, srv, ws.ID, col.ID, "Delivered once")

	// Premise: the mutation actually wrote an event. Without this the
	// assertions below would pass for a drain that delivered nothing.
	if n := pendingOutboxCount(t, srv); n == 0 {
		t.Fatal("no pending outbox events after creating an item")
	}

	srv.runOutboxDrainTick()

	got := sink.deliveries()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1: %+v", len(got), got)
	}
	if got[0].Event != kernelevents.ItemCreated {
		t.Errorf("event = %q, want %q", got[0].Event, kernelevents.ItemCreated)
	}
	if got[0].ID == "" {
		t.Error("delivered envelope carries no id — consumers dedupe on it")
	}
	var snapshot map[string]any
	if err := json.Unmarshal(got[0].Data, &snapshot); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if snapshot["id"] != item.ID {
		t.Errorf("payload id = %v, want the created item %s", snapshot["id"], item.ID)
	}

	if n := pendingOutboxCount(t, srv); n != 0 {
		t.Errorf("pending after drain = %d, want 0 — a delivered event stayed pending and will re-deliver forever", n)
	}

	// And a second tick delivers nothing: the ack is what stops it.
	srv.runOutboxDrainTick()
	if got := sink.deliveries(); len(got) != 1 {
		t.Errorf("deliveries after a second tick = %d, want still 1", len(got))
	}
}

// TestOutboxDrain_TransientFailureLeavesTheEventPending: the durable retry is
// the drain's, and it is the reason the outbox exists. A 5xx must not ack.
func TestOutboxDrain_TransientFailureLeavesTheEventPending(t *testing.T) {
	srv, sink, ws := drainFixture(t)
	col := createDrainCollection(t, srv, ws.ID)
	createDrainItem(t, srv, ws.ID, col.ID, "Retried")

	sink.setStatus(http.StatusInternalServerError)
	srv.runOutboxDrainTick()

	if n := pendingOutboxCount(t, srv); n == 0 {
		t.Fatal("event was acked despite a 5xx — the retry the outbox exists for cannot happen")
	}

	sink.setStatus(http.StatusOK)
	srv.runOutboxDrainTick()

	if n := pendingOutboxCount(t, srv); n != 0 {
		t.Errorf("pending after the endpoint recovered = %d, want 0", n)
	}
}

// TestOutboxDrain_PermanentFailureAcks: an endpoint that rejects the delivery
// in a way no retry fixes must not hold the workspace's queue. The control leg
// is the transient test above — same shape, opposite ack.
func TestOutboxDrain_PermanentFailureAcks(t *testing.T) {
	srv, sink, ws := drainFixture(t)
	col := createDrainCollection(t, srv, ws.ID)
	createDrainItem(t, srv, ws.ID, col.ID, "Rejected")

	sink.setStatus(http.StatusBadRequest)
	srv.runOutboxDrainTick()

	if n := pendingOutboxCount(t, srv); n != 0 {
		t.Errorf("pending after a 4xx = %d, want 0 — re-sending to an endpoint that rejects it costs the queue its progress", n)
	}
}

func createDrainCollection(t *testing.T, srv *Server, workspaceID string) *models.Collection {
	t.Helper()
	col, err := srv.store.CreateCollection(workspaceID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks",
		Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	return col
}

func createDrainItem(t *testing.T, srv *Server, workspaceID, collectionID, title string) *models.Item {
	t.Helper()
	item, err := srv.store.CreateItem(workspaceID, collectionID, models.ItemCreate{
		Title: title, Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	return item
}

// TestOutboxDrain_FoldsABatchIntoOneWireEvent is the F2 payoff: a lane-wide
// bulk action must reach a webhook consumer as ONE delivery carrying every
// member, not as N single-item deliveries.
func TestOutboxDrain_FoldsABatchIntoOneWireEvent(t *testing.T) {
	srv, sink, ws := drainFixture(t)
	col := createDrainCollection(t, srv, ws.ID)

	const batch = "batch-fold"
	var ids []string
	for _, title := range []string{"One", "Two", "Three"} {
		item := createDrainItem(t, srv, ws.ID, col.ID, title)
		ids = append(ids, item.ID)
	}
	// Drain the creates so only the batch is left to assert on.
	srv.runOutboxDrainTick()
	sinkBefore := len(sink.deliveries())

	for _, id := range ids {
		if err := srv.store.DeleteItem(id, store.WithEventBatch(batch)); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}
	if err := srv.store.EmitBulkHeaderEvent(ws.ID, batch, "archive", ids, nil); err != nil {
		t.Fatalf("emit header: %v", err)
	}
	// Premise: four rows are pending — three members and the header. If the
	// fold were tested against fewer, "one delivery" would prove nothing.
	if n := pendingOutboxCount(t, srv); n != 4 {
		t.Fatalf("pending = %d, want 3 members + 1 header", n)
	}

	srv.runOutboxDrainTick()

	got := sink.deliveries()[sinkBefore:]
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 folded batch event: %+v", len(got), got)
	}
	if got[0].Event != kernelevents.ItemBulkUpdated {
		t.Errorf("event = %q, want %q", got[0].Event, kernelevents.ItemBulkUpdated)
	}

	var payload struct {
		BatchID string            `json:"batch_id"`
		Op      string            `json:"op"`
		Count   int               `json:"count"`
		ItemIDs []string          `json:"item_ids"`
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(got[0].Data, &payload); err != nil {
		t.Fatalf("decode folded payload: %v", err)
	}
	if payload.BatchID != batch {
		t.Errorf("batch_id = %q, want %q — it is the consumer's correlation key when a batch splits", payload.BatchID, batch)
	}
	if payload.Op != "archive" {
		t.Errorf("op = %q, want archive — the shared delta the drain cannot derive", payload.Op)
	}
	if payload.Count != 3 || len(payload.ItemIDs) != 3 {
		t.Errorf("count/item_ids = %d/%v, want 3", payload.Count, payload.ItemIDs)
	}
	if len(payload.Members) != 3 {
		t.Errorf("members = %d, want the three member snapshots folded in", len(payload.Members))
	}

	if n := pendingOutboxCount(t, srv); n != 0 {
		t.Errorf("pending after the fold = %d, want 0 — the members must be acked with the header they were folded into", n)
	}
}

// TestOutboxDrain_MembersWithoutTheirHeaderDeliverIndividually pins the window
// SPEC-3 v1.6 defines rather than papers over: a tick can claim some members
// before the header lands.
//
// The members deliver individually, and the header that arrives afterwards
// folds whatever is LEFT. A consumer therefore sees singles plus a partial
// batch for one operation — correlated by the batch id both carry, which is
// why batch_id is on the wire at all.
func TestOutboxDrain_MembersWithoutTheirHeaderDeliverIndividually(t *testing.T) {
	srv, sink, ws := drainFixture(t)
	col := createDrainCollection(t, srv, ws.ID)

	const batch = "batch-split"
	var ids []string
	for _, title := range []string{"One", "Two"} {
		item := createDrainItem(t, srv, ws.ID, col.ID, title)
		ids = append(ids, item.ID)
	}
	srv.runOutboxDrainTick()
	sinkBefore := len(sink.deliveries())

	// The loop commits its members...
	for _, id := range ids {
		if err := srv.store.DeleteItem(id, store.WithEventBatch(batch)); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}
	// ...and a drain tick lands before the header does.
	srv.runOutboxDrainTick()

	mid := sink.deliveries()[sinkBefore:]
	if len(mid) != 2 {
		t.Fatalf("deliveries before the header = %d, want the 2 members individually: %+v", len(mid), mid)
	}
	for _, d := range mid {
		if d.Event != kernelevents.ItemDeleted {
			t.Errorf("event = %q, want %q — members deliver as their own canonical events", d.Event, kernelevents.ItemDeleted)
		}
		// THE ENVELOPE, not the payload: these are ordinary item.deleted
		// events whose payload is an item snapshot with no batch anywhere in
		// it. If the correlation is not on the envelope, a consumer receiving
		// singles-then-partial-batch cannot tell they were one operation —
		// which is the only reason batch_id is on the wire (codex round 2
		// found this claimed in comments and reachable only from the folded
		// half).
		if d.BatchID != batch {
			t.Errorf("member delivery carries batch_id %q, want %q", d.BatchID, batch)
		}
	}

	// The header arrives and folds what is left, which is nothing: the members
	// were already delivered and acked.
	if err := srv.store.EmitBulkHeaderEvent(ws.ID, batch, "archive", ids, nil); err != nil {
		t.Fatalf("emit header: %v", err)
	}
	srv.runOutboxDrainTick()

	after := sink.deliveries()[sinkBefore+len(mid):]
	if len(after) != 1 {
		t.Fatalf("deliveries after the header = %d, want the header alone: %+v", len(after), after)
	}
	var payload struct {
		BatchID string            `json:"batch_id"`
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(after[0].Data, &payload); err != nil {
		t.Fatalf("decode header payload: %v", err)
	}
	if payload.BatchID != batch {
		t.Errorf("payload batch_id = %q, want %q", payload.BatchID, batch)
	}
	if after[0].BatchID != batch {
		t.Errorf("header envelope batch_id = %q, want %q — the singles carry it on the envelope, so the batch must too or they cannot be matched", after[0].BatchID, batch)
	}
	if len(payload.Members) != 0 {
		t.Errorf("members = %d, want 0 — a fold must not re-include members a previous tick already delivered", len(payload.Members))
	}
	if n := pendingOutboxCount(t, srv); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

// TestOutboxDrain_RetentionRefusesANonPositiveWindow pins the guard on the one
// configuration mistake that is unrecoverable.
//
// A zero window makes the retention cutoff `now`, which deletes every row —
// including the PENDING ones that were never delivered. This is not
// hypothetical: the drain's defaults were originally applied only in
// StartOutboxDrain, so a tick reached directly ran with a zero max age and
// wiped the pending set on its first pass. The defaults now live in one
// resolver, and this asserts the second line of defence behind it.
func TestOutboxDrain_RetentionRefusesANonPositiveWindow(t *testing.T) {
	srv, _, ws := drainFixture(t)
	col := createDrainCollection(t, srv, ws.ID)
	createDrainItem(t, srv, ws.ID, col.ID, "Still owed")

	before := pendingOutboxCount(t, srv)
	if before == 0 {
		t.Fatal("no pending events to protect — the assertion below would prove nothing")
	}

	// ASSERT THE REFUSAL, not the survival. The first version of this test
	// checked that the pending row was still there, and it passed with the
	// guard removed: RFC3339 is second-granular, so a row written in the same
	// second as a zero-window cutoff survives `occurred_at < cutoff` whether
	// or not anything refused to run. The end state was reachable by another
	// mechanism (CONVE-12) — and worse, by a clock.
	if err := srv.runOutboxRetention(0, 0); err == nil {
		t.Fatal("a zero retention window was accepted; its cutoff is `now`, which deletes every row including undelivered ones")
	}

	// The positive control: a sane window is not refused.
	if err := srv.runOutboxRetention(time.Hour, 24*time.Hour); err != nil {
		t.Errorf("a positive window was refused: %v", err)
	}
	if after := pendingOutboxCount(t, srv); after != before {
		t.Errorf("pending went from %d to %d — the recent event was pruned by a window it is well inside", before, after)
	}
}

// TestOutboxDrain_SettingsResolveToPositiveDefaults is the primary defence the
// guard above backs up: every knob has a usable value no matter which entry
// point ran first. A zero max age is not a small misconfiguration — it makes
// the retention cutoff `now`.
func TestOutboxDrain_SettingsResolveToPositiveDefaults(t *testing.T) {
	srv := testServer(t)
	got := srv.resolveOutboxDrainSettings()

	if got.interval <= 0 || got.limit <= 0 || got.lease <= 0 {
		t.Errorf("interval/limit/lease = %v/%d/%v, want positive defaults", got.interval, got.limit, got.lease)
	}
	if got.dispatchedRetention <= 0 || got.undispatchedMaxAge <= 0 {
		t.Errorf("retention windows = %v/%v, want positive defaults", got.dispatchedRetention, got.undispatchedMaxAge)
	}
	if got.drainerID == "" {
		t.Error("drainer id is empty — every claim would be attributed to the same anonymous drainer")
	}

	// Resolution is stable: a second call must not mint a new drainer id, or
	// an instance would lose track of its own claims between passes.
	if again := srv.resolveOutboxDrainSettings(); again.drainerID != got.drainerID {
		t.Errorf("drainer id changed between passes: %q then %q", got.drainerID, again.drainerID)
	}
}
