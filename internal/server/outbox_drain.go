package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// The outbox drain: the half of SPEC-3's choke point that turns stored events
// into delivered ones.
//
// TASK-2658 built the fill side — every canonical mutation writes its event in
// the same transaction as the row it describes, so a committed mutation cannot
// lose its event. Until this file, nothing read those rows: the table filled
// and webhooks still fired from hand-calls in the handlers. This drains it.
//
// SCOPE, because the other half is deliberately absent: the drain owns the
// WEBHOOK surface only. SSE stays direct-published at the mutation site
// (SPEC-3 v1.5) — it carries request-scoped attribution a frozen payload does
// not hold, and putting the live UI behind a polling drain is a latency change
// tracked as its own item. What both surfaces now share is the NAME, derived
// from the taxonomy.
const (
	defaultOutboxDrainInterval = 5 * time.Second

	// How many pending rows one pass claims. Whole batches are claimed past
	// this bound (see ClaimPendingOutboxEvents) — it is a throughput knob, not
	// a statement about what a bulk operation was.
	defaultOutboxDrainLimit = 100

	// How long a claim is honored before another instance may take the row.
	// Generous relative to a delivery: three attempts with linear backoff and
	// a 10s per-attempt timeout is well under a minute, and re-claiming early
	// only buys a duplicate.
	defaultOutboxClaimLease = 5 * time.Minute

	// Retention. Dispatched rows are kept as the durable record that an event
	// existed; undispatched rows are kept far longer because they are still
	// owed, but NOT forever — see PruneUndispatchedOutbox for why the privacy
	// claim is what makes this bound mandatory rather than tidy.
	defaultOutboxDispatchedRetention = 24 * time.Hour
	defaultOutboxUndispatchedMaxAge  = 7 * 24 * time.Hour
)

type outboxDrainConfig struct {
	mu                  sync.Mutex
	interval            time.Duration
	limit               int
	lease               time.Duration
	dispatchedRetention time.Duration
	undispatchedMaxAge  time.Duration
	stop                chan struct{}
	running             bool
	drainerID           string
	// tick, when non-nil, replaces the interval ticker so a test can pin
	// assertions to a SPECIFIC pass instead of racing a free-running loop —
	// the same affordance server.go's other sweepers expose.
	tick <-chan time.Time
}

// SetOutboxDrainConfig overrides the drain's timings. Zero values keep the
// defaults, so a caller can set one knob without restating the rest.
func (s *Server) SetOutboxDrainConfig(interval, lease, dispatchedRetention, undispatchedMaxAge time.Duration, limit int) {
	s.outboxDrain.mu.Lock()
	defer s.outboxDrain.mu.Unlock()
	if interval > 0 {
		s.outboxDrain.interval = interval
	}
	if lease > 0 {
		s.outboxDrain.lease = lease
	}
	if dispatchedRetention > 0 {
		s.outboxDrain.dispatchedRetention = dispatchedRetention
	}
	if undispatchedMaxAge > 0 {
		s.outboxDrain.undispatchedMaxAge = undispatchedMaxAge
	}
	if limit > 0 {
		s.outboxDrain.limit = limit
	}
}

// resolveOutboxDrainSettings fills every unset knob with its default and
// stores the result, so the resolved values are identical no matter which
// entry point ran first.
//
// ONE RESOLVER, NOT DEFAULTS-AT-START. When the defaults were applied only in
// StartOutboxDrain, a tick reached directly — every test does this, and so
// would any future admin-triggered drain — ran with a ZERO undispatched max
// age, which made the retention cutoff `now` and deleted the entire pending
// set on the first pass. A config whose zero value is catastrophic must not
// depend on a particular caller having normalized it.
func (s *Server) resolveOutboxDrainSettings() outboxDrainSettings {
	s.outboxDrain.mu.Lock()
	defer s.outboxDrain.mu.Unlock()
	if s.outboxDrain.interval <= 0 {
		s.outboxDrain.interval = defaultOutboxDrainInterval
	}
	if s.outboxDrain.limit <= 0 {
		s.outboxDrain.limit = defaultOutboxDrainLimit
	}
	if s.outboxDrain.lease <= 0 {
		s.outboxDrain.lease = defaultOutboxClaimLease
	}
	if s.outboxDrain.dispatchedRetention <= 0 {
		s.outboxDrain.dispatchedRetention = defaultOutboxDispatchedRetention
	}
	if s.outboxDrain.undispatchedMaxAge <= 0 {
		s.outboxDrain.undispatchedMaxAge = defaultOutboxUndispatchedMaxAge
	}
	if s.outboxDrain.drainerID == "" {
		// Per-process identity. Not persisted deliberately: a restarted
		// process must NOT inherit its predecessor's claims — those rows are
		// exactly the ones whose lease should decide their fate.
		s.outboxDrain.drainerID = uuid.NewString()
	}
	return outboxDrainSettings{
		interval:            s.outboxDrain.interval,
		limit:               s.outboxDrain.limit,
		lease:               s.outboxDrain.lease,
		dispatchedRetention: s.outboxDrain.dispatchedRetention,
		undispatchedMaxAge:  s.outboxDrain.undispatchedMaxAge,
		drainerID:           s.outboxDrain.drainerID,
	}
}

type outboxDrainSettings struct {
	interval            time.Duration
	limit               int
	lease               time.Duration
	dispatchedRetention time.Duration
	undispatchedMaxAge  time.Duration
	drainerID           string
}

// StartOutboxDrain starts the periodic drain loop. Idempotent; tracked by
// Server.bg so Stop() drains it before the process exits (the BUG-842
// invariant every sweeper here observes).
func (s *Server) StartOutboxDrain() {
	settings := s.resolveOutboxDrainSettings()

	s.outboxDrain.mu.Lock()
	if s.outboxDrain.running {
		s.outboxDrain.mu.Unlock()
		return
	}
	s.outboxDrain.stop = make(chan struct{})
	s.outboxDrain.running = true
	interval := settings.interval
	stop := s.outboxDrain.stop
	tick := s.outboxDrain.tick
	s.outboxDrain.mu.Unlock()

	slog.Info("outbox drain started", "interval", interval.String())

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("outbox-drain")
		var c <-chan time.Time
		if tick != nil {
			c = tick
		} else {
			t := time.NewTicker(interval)
			defer t.Stop()
			c = t.C
		}
		for {
			select {
			case <-stop:
				return
			case <-c:
				s.runOutboxDrainTick()
			}
		}
	}()
}

// stopOutboxDrain signals the loop to exit. Safe when it never started.
func (s *Server) stopOutboxDrain() {
	s.outboxDrain.mu.Lock()
	defer s.outboxDrain.mu.Unlock()
	if !s.outboxDrain.running {
		return
	}
	close(s.outboxDrain.stop)
	s.outboxDrain.running = false
}

// runOutboxDrainTick is one pass: claim, deliver, ack, retain.
func (s *Server) runOutboxDrainTick() {
	if s.store == nil {
		return
	}
	settings := s.resolveOutboxDrainSettings()

	leaseCutoff := time.Now().UTC().Add(-settings.lease).Format(time.RFC3339)
	events, err := s.store.ClaimPendingOutboxEvents(settings.drainerID, settings.limit, leaseCutoff)
	if err != nil {
		slog.Error("outbox drain: claim failed", "error", err)
		return
	}
	// Report what the claim REFUSED to read. The claim excludes rows over
	// store.MaxOutboxClaimableBytes in its own predicate so they cannot occupy
	// candidate slots (BUG-2827); without this they would then be invisible
	// until retention silently reaped them. Only a binary older than that cap
	// can have written one, so on most instances this logs nothing, ever.
	//
	// Logged rather than dead-lettered: stamping dispatched_at would record
	// that an event went out when it did not, in the table that is the only
	// durable answer to "did this mutation emit?". Leaving the row pending
	// keeps that answer honest and hands its lifecycle to the undispatched
	// retention bound, which already exists for events that can never be
	// delivered.
	if oversized, oerr := s.store.OversizedPendingOutbox(5); oerr != nil {
		slog.Error("outbox drain: oversized scan failed", "error", oerr)
	} else {
		for _, row := range oversized {
			slog.Error("outbox drain: payload over the size limit, not claimed",
				"event_id", row.ID, "event_type", row.EventType,
				"bytes", row.Bytes, "limit", store.MaxOutboxClaimableBytes)
		}
	}
	for _, unit := range groupOutboxDeliveries(events) {
		s.deliverOutboxUnit(unit)
	}

	if err := s.runOutboxRetention(settings.dispatchedRetention, settings.undispatchedMaxAge, settings.lease); err != nil {
		slog.Error("outbox drain: retention pass failed", "error", err)
	}
}

// outboxDelivery is one WIRE event and the rows it acks.
//
// The distinction the drain turns on: a folded batch is many rows and ONE
// delivery, so acking has to be per-unit rather than per-row or a partially
// acked batch would re-deliver on the next pass.
type outboxDelivery struct {
	workspaceID string
	eventType   string
	eventID     string
	batchID     string
	occurredAt  string
	payload     []byte
	rowIDs      []string
	// claimToken is the claim these rows were taken under. Ack and release are
	// conditioned on it, so a pass whose lease expired mid-delivery cannot
	// stamp or free rows another instance now holds.
	claimToken string
}

// groupOutboxDeliveries turns claimed rows into wire deliveries, folding each
// batch that has a header into one.
//
// THE FOLD RULE (SPEC-3 v1.6): header PLUS whatever member rows of that batch
// are still undispatched. Members of a batch whose header is not in this claim
// deliver individually — that is not a fallback, it is the defined behaviour
// for the window between the member loop committing and the header landing. A
// consumer can therefore see singles and then a partial batch for one
// operation, correlated by the batch_id both carry.
func groupOutboxDeliveries(events []store.OutboxEvent) []outboxDelivery {
	headers := map[string]store.OutboxEvent{}
	members := map[string][]store.OutboxEvent{}
	var singles []store.OutboxEvent

	for _, ev := range events {
		switch {
		case ev.BatchID == "":
			singles = append(singles, ev)
		case ev.EventType == kernelevents.ItemBulkUpdated:
			headers[ev.BatchID] = ev
		default:
			members[ev.BatchID] = append(members[ev.BatchID], ev)
		}
	}

	var out []outboxDelivery
	for _, ev := range singles {
		out = append(out, singleOutboxDelivery(ev))
	}
	for batch, member := range members {
		if _, folded := headers[batch]; folded {
			continue
		}
		for _, ev := range member {
			out = append(out, singleOutboxDelivery(ev))
		}
	}
	for batch, header := range headers {
		rowIDs := []string{header.ID}
		var payloads [][]byte
		for _, ev := range members[batch] {
			rowIDs = append(rowIDs, ev.ID)
			payloads = append(payloads, ev.Payload)
		}
		folded, err := store.FoldBulkHeader(header.Payload, payloads)
		if err != nil {
			// A header we cannot parse is not deliverable as a batch. Fall
			// back to the header's own payload rather than dropping the
			// event: the members are still acked with it, and a consumer
			// receiving the unfolded header at least learns the operation
			// happened.
			slog.Error("outbox drain: fold failed", "batch", batch, "error", err)
			folded = header.Payload
		}
		out = append(out, outboxDelivery{
			workspaceID: header.WorkspaceID,
			eventType:   header.EventType,
			eventID:     header.ID,
			batchID:     header.BatchID,
			occurredAt:  header.OccurredAt,
			payload:     folded,
			rowIDs:      rowIDs,
			claimToken:  header.ClaimToken,
		})
	}
	return out
}

func singleOutboxDelivery(ev store.OutboxEvent) outboxDelivery {
	return outboxDelivery{
		workspaceID: ev.WorkspaceID,
		eventType:   ev.EventType,
		eventID:     ev.ID,
		// Carried even when this member is delivered ALONE: a consumer that
		// receives members before their header needs the correlation on the
		// singles, not only on the batch event.
		batchID:    ev.BatchID,
		occurredAt: ev.OccurredAt,
		claimToken: ev.ClaimToken,
		payload:    ev.Payload,
		rowIDs:     []string{ev.ID},
	}
}

// deliverOutboxUnit puts one wire event on the webhook surface and acks or
// releases its rows according to the outcome.
func (s *Server) deliverOutboxUnit(unit outboxDelivery) {
	if s.webhooks == nil {
		// No dispatcher configured (self-hosted with webhooks off, most
		// tests). The event has nowhere to go and is not owed to anyone, so
		// ack it — leaving it pending would grow the table forever and then
		// hand the retention bound a queue of events nobody ever wanted.
		s.ackOutboxRows(unit.claimToken, unit.rowIDs)
		return
	}

	outcome, err := s.webhooks.DeliverEvent(webhooks.Delivery{
		WorkspaceID: unit.workspaceID,
		EventID:     unit.eventID,
		BatchID:     unit.batchID,
		Event:       unit.eventType,
		OccurredAt:  unit.occurredAt,
		Payload:     json.RawMessage(unit.payload),
	})
	if err != nil {
		// The server's own failure: nothing was attempted, so the event is
		// still owed in full.
		s.failOutboxRows(unit.claimToken, unit.rowIDs, err.Error())
		return
	}
	if outcome.Retryable() {
		s.failOutboxRows(unit.claimToken, unit.rowIDs, outcome.LastError)
		return
	}
	s.ackOutboxRows(unit.claimToken, unit.rowIDs)
}

func (s *Server) ackOutboxRows(claimToken string, ids []string) {
	if err := s.store.MarkOutboxDispatched(claimToken, ids); err != nil {
		// Not acking is safe — at-least-once means the next pass re-delivers.
		slog.Error("outbox drain: ack failed", "error", err)
	}
}

func (s *Server) failOutboxRows(claimToken string, ids []string, reason string) {
	for _, id := range ids {
		if err := s.store.MarkOutboxAttemptFailed(claimToken, id, reason); err != nil {
			slog.Error("outbox drain: recording a failed attempt failed", "id", id, "error", err)
		}
	}
}

// runOutboxRetention enforces both halves of the retention window.
//
// Both, on every tick, because they bound different things: dispatched rows
// are history and undispatched rows are FROZEN PAYLOADS that de-identification
// cannot reach. The second is the privacy bound SPEC-3 makes temporal, and it
// is the one that would be easy to leave out — PruneDispatchedOutbox looks
// like it covers retention until you notice which rows it can never see.
// The error return exists FOR THE TEST, and is worth the signature: a refusal
// that only logs is indistinguishable from a prune that happened to match no
// rows, which is exactly how the first version of this guard's test passed
// against a build with the guard removed (RFC3339 is second-granular, so a
// row written in the same second as the cutoff survives `occurred_at <
// cutoff` either way). Returning the refusal makes the guard assert-able
// without depending on the clock.
func (s *Server) runOutboxRetention(dispatchedRetention, undispatchedMaxAge, lease time.Duration) error {
	// A non-positive window would make the cutoff `now` and delete the entire
	// table — the pending set included. Refuse rather than normalize: reaching
	// here with a zero means a caller bypassed resolveOutboxDrainSettings, and
	// quietly substituting a default would hide that from whoever added the
	// bypass.
	if dispatchedRetention <= 0 || undispatchedMaxAge <= 0 || lease <= 0 {
		err := fmt.Errorf("refusing to prune with a non-positive retention window (dispatched %s, undispatched %s, lease %s)",
			dispatchedRetention, undispatchedMaxAge, lease)
		slog.Error("outbox drain: " + err.Error())
		return err
	}
	dispatchedBefore := time.Now().UTC().Add(-dispatchedRetention).Format(time.RFC3339)
	if _, err := s.store.PruneDispatchedOutbox(dispatchedBefore); err != nil {
		slog.Error("outbox drain: pruning dispatched events failed", "error", err)
	}

	undispatchedBefore := time.Now().UTC().Add(-undispatchedMaxAge).Format(time.RFC3339)
	// The same lease cutoff the claim uses: a row another instance is
	// currently delivering must survive this pass.
	leaseCutoff := time.Now().UTC().Add(-lease).Format(time.RFC3339)
	n, err := s.store.PruneUndispatchedOutbox(undispatchedBefore, leaseCutoff)
	if err != nil {
		slog.Error("outbox drain: pruning undispatched events failed", "error", err)
		return err
	}
	if n > 0 {
		// Loud on purpose: these events were owed and never delivered. The
		// count is the only place a delivery problem that has been failing for
		// the whole window becomes visible.
		slog.Warn("outbox drain: dropped undeliverable events at max age",
			"count", n, "max_age", undispatchedMaxAge.String())
	}
	return nil
}
