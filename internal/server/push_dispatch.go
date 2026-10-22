package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The push dispatcher (TASK-3939 part B1): claims settled user_notifications
// rows, runs every privacy gate (push_gates.go) on each, groups the survivors
// per (user, item), sends one content-free push per live device, and settles
// the rows. Part B2a added the 30/h cap and the summary push, the retry matrix
// (which results retry, skip or delete a device), and the expire and retention
// sweeps. Server startup (cmd/pad configurePush, TASK-3939 B2c) calls
// StartPushDispatch when PAD_PUSH_GATEWAY_URL is set, after applying the
// PAD_PUSH_DISPATCH_* timing knobs through SetPushDispatchConfig (TASK-3939
// part C); an unset knob keeps its default below.
//
// LEASE INVARIANT (r6-Q1): lease >= sendTimeout + leaseMargin. A claim is
// stamped with second precision (up to 1s behind the real clock), and a Send
// can take sendTimeout (notify.SendTimeout), so a lease shorter than that lets
// another instance reclaim a row mid-send and push it twice. It is enforced
// three ways: the setter refuses a violating config, all gateway I/O runs
// under a deadline of start+lease-leaseMargin, and a chunk only starts when
// sendTimeout is still left before that deadline (otherwise its rows are
// unclaimed, no attempt burned). Startup applies the operator's knobs through
// the setter, so a lease too short for sendTimeout (fixed at notify.SendTimeout,
// not configurable) plus the margin refuses startup (TASK-3939 part C).
//
// SENT BUT UNRECORDED (TASK-4032): a failed 'sent' record is not merely logged.
// The push went out, so the row would be reclaimed after the lease and pushed
// again; the ids are parked in-process, the record is retried at every tick
// start (gateway or not), parked rows are never re-sent and count toward the
// 30/h cap, and an entry is dropped once recorded or past pushExpireAfter.
// The set is in-process, so a restart or a second instance can still re-send a
// row once after its lease, which the client de-dupes by nid.
//
// Logging: fixed strings and counts only. A store error can carry a row id or
// a value, so it is never logged here.
const (
	defaultPushInterval    = 5 * time.Second
	defaultPushSettle      = 10 * time.Second
	defaultPushLease       = 60 * time.Second
	defaultPushLeaseMargin = 15 * time.Second

	pushSummaryAbove = 3                  // more than this many groups for one user => one summary push
	pushRateCap      = 30                 // rows pushed per user per pushRateWindow
	pushRateWindow   = time.Hour          //
	pushExpireAfter  = time.Hour          // a pending row older than this is noise, not a push
	pushRetention    = 7 * 24 * time.Hour // sent, skipped and expired rows are kept this long
	pushPruneEvery   = time.Hour          // created_at is unindexed, so the prune is throttled

	// pushUnrecordedCap bounds the parked set in size (TASK-4032). The age
	// rule alone bounds it in time. At the cap the dispatcher stops claiming
	// and sending (backpressure) rather than evicting: an evicted row is
	// pending, and would be sent again once its lease lapsed.
	pushUnrecordedCap = 1000

	skipRateLimited     = "rate_limited"
	skipGatewayRejected = "gateway_rejected"
)

var errPushLeaseTooShort = errors.New("push dispatch: lease must be at least sendTimeout + leaseMargin")

// pushSender is the gateway as the dispatcher sees it; *notify.Gateway
// satisfies it, and tests substitute a fake.
type pushSender interface {
	Send(ctx context.Context, msgs []notify.Message) ([]notify.Result, error)
}

type pushDispatchConfig struct {
	mu          sync.Mutex
	interval    time.Duration
	settle      time.Duration
	lease       time.Duration
	leaseMargin time.Duration
	sendTimeout time.Duration
	sender      pushSender       // test seam; nil = the server's gateway
	now         func() time.Time // test seam; nil = time.Now
	stop        chan struct{}
	running     bool
	// startedInterval is the interval the ticker was last created with, 0 if
	// the loop never started (TASK-3939 part C: proves the knob reached Start).
	startedInterval time.Duration
	instanceID      string
	lastPrune       time.Time // when the retention sweep last ran
	// unrecorded holds rows the gateway accepted whose 'sent' record failed
	// (TASK-4032); see parkUnrecorded.
	unrecorded map[string]unrecordedRow
	// unrecordedCapOverride replaces pushUnrecordedCap when > 0 (tests only).
	unrecordedCapOverride int
	// unrecordedFull is whether the last tick found the set at its cap, so
	// the full state logs once on entering and once on leaving.
	unrecordedFull bool
	// tickMu serializes ticks for the life of the process: Stop does not wait
	// for the tick in flight, so a later Start could otherwise run a second
	// one beside it, and two ticks would each pass the cap check and send.
	// Stop cancels the in-flight tick's context, so a waiter is never held
	// longer than that tick's unwind.
	tickMu sync.Mutex
	// tick, when non-nil, replaces the interval ticker so a test drives passes.
	tick <-chan time.Time
}

// unrecordedRow is a row whose push the gateway accepted but whose 'sent'
// record has not landed.
type unrecordedRow struct {
	userID  string
	created time.Time // the notification's created_at; its expiry clock
}

type pushDispatchSettings struct {
	interval, settle, lease, leaseMargin, sendTimeout time.Duration
	instanceID                                        string
}

// SetPushDispatchConfig overrides the timings; zero keeps the current value.
// Server startup calls it from the PAD_PUSH_DISPATCH_* knobs (cmd/pad
// configurePush, TASK-3939 part C) and passes 0 for sendTimeout, which has no
// knob because notify.SendTimeout is baked into the gateway's HTTP client.
// interval is read once, by StartPushDispatch (the ticker is fixed at Start,
// so a later change needs a restart); settle, lease, leaseMargin and
// sendTimeout are read on every tick.
// A combination violating the lease invariant is refused whole and nothing
// changes; the error wraps errPushLeaseTooShort and states the RESOLVED lease,
// send timeout and margin, so a caller can tell which knob caused it.
func (s *Server) SetPushDispatchConfig(interval, settle, lease, leaseMargin, sendTimeout time.Duration) error {
	cur := s.resolvePushDispatchSettings()
	pick := func(v, d time.Duration) time.Duration {
		if v > 0 {
			return v
		}
		return d
	}
	next := pushDispatchSettings{
		interval: pick(interval, cur.interval), settle: pick(settle, cur.settle),
		lease: pick(lease, cur.lease), leaseMargin: pick(leaseMargin, cur.leaseMargin),
		sendTimeout: pick(sendTimeout, cur.sendTimeout),
	}
	// sendTimeout + leaseMargin wraps negative when the margin is within
	// sendTimeout of MaxInt64 (ParseDuration accepts up to about 2562047h), and
	// the wrapped sum would pass the comparison below. Such a margin exceeds
	// every representable lease, so it is the same violation (TASK-3939 part C,
	// ruling 37e3eafa). Every other use of these durations is time.Time.Add or
	// NewTicker, which cannot wrap at this magnitude, so no cap is needed.
	if next.leaseMargin > math.MaxInt64-next.sendTimeout || next.lease < next.sendTimeout+next.leaseMargin {
		slog.Warn("push dispatch: config refused", "reason", "lease shorter than send timeout plus margin")
		return fmt.Errorf("%w: lease %s is shorter than send timeout %s plus margin %s", errPushLeaseTooShort, next.lease, next.sendTimeout, next.leaseMargin)
	}
	d := &s.pushDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	d.interval, d.settle, d.lease, d.leaseMargin, d.sendTimeout = next.interval, next.settle, next.lease, next.leaseMargin, next.sendTimeout
	return nil
}

// SetPushSender substitutes the gateway client (a test seam).
func (s *Server) SetPushSender(p pushSender) {
	s.pushDispatch.mu.Lock()
	defer s.pushDispatch.mu.Unlock()
	s.pushDispatch.sender = p
}

// resolvePushDispatchSettings fills every unset knob and stores the result,
// so a tick reached directly never sees a zero (the outbox drain's lesson).
func (s *Server) resolvePushDispatchSettings() pushDispatchSettings {
	d := &s.pushDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.interval <= 0 {
		d.interval = defaultPushInterval
	}
	if d.settle <= 0 {
		d.settle = defaultPushSettle
	}
	if d.sendTimeout <= 0 {
		d.sendTimeout = notify.SendTimeout
	}
	if d.leaseMargin <= 0 {
		d.leaseMargin = defaultPushLeaseMargin
	}
	if d.lease <= 0 {
		d.lease = defaultPushLease
	}
	if d.instanceID == "" {
		d.instanceID = uuid.NewString()
	}
	return pushDispatchSettings{d.interval, d.settle, d.lease, d.leaseMargin, d.sendTimeout, d.instanceID}
}

func (s *Server) pushNow() time.Time {
	s.pushDispatch.mu.Lock()
	now := s.pushDispatch.now
	s.pushDispatch.mu.Unlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

func (s *Server) pushSenderOrNil() pushSender {
	s.pushDispatch.mu.Lock()
	defer s.pushDispatch.mu.Unlock()
	if s.pushDispatch.sender != nil {
		return s.pushDispatch.sender
	}
	if s.pushGateway != nil {
		return s.pushGateway
	}
	return nil
}

// StartPushDispatch starts the dispatch loop. Idempotent; tracked on s.bg.
func (s *Server) StartPushDispatch() {
	cfg := s.resolvePushDispatchSettings()
	d := &s.pushDispatch
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.stop = make(chan struct{})
	d.running = true
	stop, tick := d.stop, d.tick
	d.startedInterval = cfg.interval
	d.mu.Unlock()

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("push-dispatch")
		// A stop cancels the tick in flight, which unclaims what it holds.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		var c <-chan time.Time
		if tick != nil {
			c = tick
		} else {
			t := time.NewTicker(cfg.interval)
			defer t.Stop()
			c = t.C
		}
		for {
			select {
			case <-stop:
				return
			case <-c:
				s.runPushDispatchTick(ctx)
			}
		}
	}()
}

// PushDispatchRunning reports whether the dispatch loop has been started and
// not stopped. Read-only; startup's wiring test reads it (TASK-3939 B2c).
func (s *Server) PushDispatchRunning() bool {
	s.pushDispatch.mu.Lock()
	defer s.pushDispatch.mu.Unlock()
	return s.pushDispatch.running
}

// PushDispatchApplied is what the dispatcher will use (or is using). Interval
// is the stored setting; StartedInterval is the interval the ticker was
// actually created with, 0 if the loop never started.
type PushDispatchApplied struct {
	Interval, Settle, Lease, LeaseMargin, StartedInterval time.Duration
}

// PushDispatchApplied reads the dispatcher's timings, filling defaults for
// any unset one. Read-only; startup's wiring test reads it to prove the knobs
// reached the dispatcher, and the interval before Start (TASK-3939 part C,
// ruling 77a8fae7). A test-only seam, read at single-threaded startup: it
// takes the lock twice (resolve, then startedInterval), so a concurrent
// reconfigure could give a mixed snapshot.
func (s *Server) PushDispatchApplied() PushDispatchApplied {
	cur := s.resolvePushDispatchSettings()
	s.pushDispatch.mu.Lock()
	defer s.pushDispatch.mu.Unlock()
	return PushDispatchApplied{cur.interval, cur.settle, cur.lease, cur.leaseMargin, s.pushDispatch.startedInterval}
}

// stopPushDispatch signals the loop to exit. Safe when it never started.
func (s *Server) stopPushDispatch() {
	d := &s.pushDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.running {
		return
	}
	close(d.stop)
	d.running = false
}

// pushUnit is the rows of one (user, item) group after gating, which become
// one push per live device. nid and kind are the newest row's.
type pushUnit struct {
	rowIDs  []string
	nid     string
	kind    string // notify.KindSummary for a collapsed unit
	devices []store.PushDevice
}

// expireAndPrune is the tick's housekeeping, run before the claim so a stale
// row is never sent: pending rows older than pushExpireAfter expire, and rows
// settled longer ago than pushRetention are deleted at most hourly.
func (s *Server) expireAndPrune(cfg pushDispatchSettings, start time.Time) {
	if _, err := s.store.ExpireStaleNotifications(start.Add(-pushExpireAfter), start.Add(-cfg.lease)); err != nil {
		slog.Warn("push dispatch: housekeeping failed", "op", "expire")
	}
	d := &s.pushDispatch
	d.mu.Lock()
	prev := d.lastPrune
	due := start.Sub(prev) >= pushPruneEvery
	if due {
		d.lastPrune = start
	}
	d.mu.Unlock()
	if !due {
		return
	}
	if _, err := s.store.PruneNotifications(start.Add(-pushRetention)); err != nil {
		slog.Warn("push dispatch: housekeeping failed", "op", "prune")
		d.mu.Lock()
		d.lastPrune = prev
		d.mu.Unlock()
	}
}

// parkUnrecorded remembers rows the gateway delivered but whose 'sent' record
// failed (TASK-4032). A row left pending is reclaimed after the lease and
// pushed again, once per lease, until it expires; the set stops that
// in-process (the tick skips parked rows after the claim) and drives the
// record until it lands. meta supplies each row's user and created_at; an
// unknown row falls back to now. Nothing is ever evicted: an evicted row is
// still pending and would be re-sent. The size bound is held by the tick,
// which stops claiming and sending while the set is at its cap (see
// checkUnrecordedFull); one tick's sends can overshoot it by at most one
// batch.
func (s *Server) parkUnrecorded(ids []string, meta map[string]store.Notification, at time.Time) {
	d := &s.pushDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.unrecorded == nil {
		d.unrecorded = map[string]unrecordedRow{}
	}
	for _, id := range ids {
		e := unrecordedRow{created: at}
		if n, ok := meta[id]; ok {
			e.userID = n.UserID
			if t, err := time.Parse(time.RFC3339, n.CreatedAt); err == nil {
				e.created = t
			}
		}
		d.unrecorded[id] = e
	}
}

// recordedPrefix is how many leading ids a failed RecordNotificationsSent
// still recorded: its chunks commit one by one, and an id left parked after
// its chunk landed counts twice against the rate cap (TASK-4032 codex r3 M1).
func recordedPrefix(err error) int {
	var rse *store.RecordSentError
	if errors.As(err, &rse) {
		return rse.Landed
	}
	return 0
}

// expireCutoff is the creation time before which a row is past
// pushExpireAfter. It is truncated to the second because the store compares
// RFC3339 seconds: the claim's bound (created_at >= cutoff) and the parked
// entry's drop rule (created before cutoff) are then exact complements, so no
// row is both claimable and droppable.
func expireCutoff(start time.Time) time.Time {
	return start.Add(-pushExpireAfter).Truncate(time.Second)
}

// retryUnrecorded runs at the start of every tick, before the gateway check
// and the claim, so it needs no gateway: recording is database work. A record
// that returns no error clears every parked id, including one it changed no
// row for (that row was settled elsewhere, which is a fine outcome too). A
// failed one clears the ids in the chunks that committed before the failure
// and keeps the rest, except those whose notification is past
// pushExpireAfter (expireCutoff): the claim never selects a row created
// before that cutoff, whether or not the expiry sweep landed (TASK-4032 r5
// H-Q1), so nothing can re-send it, and dropping the entry is what keeps a
// database that stays down from growing the set. The ids are sorted so the
// chunking is deterministic.
func (s *Server) retryUnrecorded(start time.Time) {
	d := &s.pushDispatch
	d.mu.Lock()
	ids := make([]string, 0, len(d.unrecorded))
	for id := range d.unrecorded {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	_, err := s.store.RecordNotificationsSent(ids)
	landed := len(ids)
	if err != nil {
		landed = recordedPrefix(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := expireCutoff(start)
	for i, id := range ids {
		if i < landed || d.unrecorded[id].created.Before(cutoff) {
			delete(d.unrecorded, id)
		}
	}
	if err != nil {
		slog.Warn("push dispatch: settle failed", "op", "sent-retry")
	}
}

// checkUnrecordedFull reports whether the parked set is at its cap, which
// holds the tick's claim and send. It logs one fixed-string warning when the
// state is entered and one when it is left, never per tick, with no ids and
// no error text.
func (s *Server) checkUnrecordedFull() bool {
	d := &s.pushDispatch
	d.mu.Lock()
	limit := pushUnrecordedCap
	if d.unrecordedCapOverride > 0 {
		limit = d.unrecordedCapOverride
	}
	full := len(d.unrecorded) >= limit
	changed := full != d.unrecordedFull
	d.unrecordedFull = full
	d.mu.Unlock()
	if changed && full {
		slog.Warn("push dispatch: unrecorded sends at the cap, holding claims and sends")
	} else if changed {
		slog.Warn("push dispatch: unrecorded sends below the cap, resuming")
	}
	return full
}

// unrecordedSnapshot returns the parked ids and each user's parked count. A
// parked row was delivered, so it counts toward that user's 30/h cap although
// the database does not show it as sent yet.
func (s *Server) unrecordedSnapshot() (ids map[string]bool, perUser map[string]int) {
	d := &s.pushDispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	ids, perUser = make(map[string]bool, len(d.unrecorded)), map[string]int{}
	for id, e := range d.unrecorded {
		ids[id] = true
		perUser[e.userID]++
	}
	return
}

// rowNewer orders two rows by created_at, then kind priority, then id.
func rowNewer(a, b store.Notification) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	if pa, pb := kindPriority[a.Kind], kindPriority[b.Kind]; pa != pb {
		return pa > pb
	}
	return a.ID > b.ID
}

// tickWarn records which gateway failures a tick saw, so each logs once.
type tickWarn struct{ unavailable, badRequest bool }

// kindPriority breaks a created_at tie between rows of one group, so the kind
// of a mixed group is deterministic (ids are random).
var kindPriority = map[string]int{
	notify.KindReminder: 0, notify.KindComment: 1, notify.KindAssigned: 2, notify.KindReply: 3, notify.KindMention: 4,
}

// runPushDispatchTick is one pass. ctx is the shutdown signal.
func (s *Server) runPushDispatchTick(ctx context.Context) {
	s.pushDispatch.tickMu.Lock()
	defer s.pushDispatch.tickMu.Unlock()
	cfg := s.resolvePushDispatchSettings()
	start := s.pushNow()
	s.expireAndPrune(cfg, start)
	s.retryUnrecorded(start)
	if s.checkUnrecordedFull() {
		return // pending rows wait, up to their expiry, rather than repeat
	}
	sender := s.pushSenderOrNil()
	if sender == nil {
		return
	}
	// Taken before the claim: parked rows are excluded from it, so they
	// cannot occupy batch slots ahead of rows that still need a push.
	parked, parkedPerUser := s.unrecordedSnapshot()
	exclude := make([]string, 0, len(parked))
	for id := range parked {
		exclude = append(exclude, id)
	}
	// Fresh per tick and never kept: the store appends its own nonce, and
	// every settle below uses Claim.Token.
	token := "pd-" + cfg.instanceID + "-" + uuid.NewString()
	claim, err := s.store.ClaimPendingNotifications(token, notify.MaxBatch, start.Add(-cfg.settle), start.Add(-cfg.lease), expireCutoff(start), exclude)
	if err != nil {
		slog.Error("push dispatch: claim failed", "op", "claim")
		return
	}
	if len(claim.Rows) == 0 {
		return
	}
	deadline := start.Add(cfg.lease - cfg.leaseMargin)

	cache := newPushGateCache()
	skips := map[string][]string{}
	var unclaim []string
	groups := map[string][]store.Notification{}
	var order []string
	meta := make(map[string]store.Notification, len(claim.Rows))
	for _, n := range claim.Rows {
		meta[n.ID] = n
		if parked[n.ID] {
			// Belt: the claim already excludes parked ids; a parked row that
			// still arrives is never gated or sent. Left claimed.
			continue
		}
		_, reason, gerr := s.notificationAccess(cache, n)
		if gerr == nil && reason == "" {
			reason, gerr = s.preferenceGates(cache, n, start)
		}
		switch {
		case gerr != nil:
			// Not a denial: never send, never record a skip, burn no attempt.
			unclaim = append(unclaim, n.ID)
		case reason != "":
			skips[reason] = append(skips[reason], n.ID)
		default:
			key := n.UserID + "/" + n.ItemID
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], n)
		}
	}
	s.settleSkips(claim.Token, skips)
	s.unclaim(claim.Token, unclaim)

	// Per user, in claim order: more than pushSummaryAbove groups collapse
	// into one summary unit (after gating, so only visible rows count), then
	// the 30/h cap admits or rate-limits each unit. Rows from the same tick
	// that were admitted count against the cap even if the send later fails.
	var userOrder []string
	byUser := map[string][]string{}
	for _, key := range order {
		u := groups[key][0].UserID
		if _, seen := byUser[u]; !seen {
			userOrder = append(userOrder, u)
		}
		byUser[u] = append(byUser[u], key)
	}
	var units []pushUnit
	limited := map[string][]string{}
	var capErr []string
	for _, uid := range userOrder {
		var userUnits []pushUnit
		for _, key := range byUser[uid] {
			rows := groups[key]
			sort.SliceStable(rows, func(i, j int) bool { return rowNewer(rows[j], rows[i]) })
			u := pushUnit{nid: rows[len(rows)-1].ID, kind: rows[len(rows)-1].Kind, devices: cache.devices[uid]}
			for _, r := range rows {
				u.rowIDs = append(u.rowIDs, r.ID)
			}
			userUnits = append(userUnits, u)
		}
		if len(userUnits) > pushSummaryAbove {
			sum := pushUnit{kind: notify.KindSummary, devices: cache.devices[uid]}
			var newest store.Notification
			for _, key := range byUser[uid] {
				for _, r := range groups[key] {
					sum.rowIDs = append(sum.rowIDs, r.ID)
					if newest.ID == "" || rowNewer(r, newest) {
						newest = r
					}
				}
			}
			sum.nid = newest.ID
			userUnits = []pushUnit{sum}
		}
		sent, cerr := s.store.CountNotificationsSentSince(uid, start.Add(-pushRateWindow))
		sent += parkedPerUser[uid]
		for _, u := range userUnits {
			switch {
			case cerr != nil:
				capErr = append(capErr, u.rowIDs...)
			case sent >= pushRateCap:
				limited[skipRateLimited] = append(limited[skipRateLimited], u.rowIDs...)
			default:
				sent += len(u.rowIDs)
				units = append(units, u)
			}
		}
	}
	s.settleSkips(claim.Token, limited)
	s.unclaim(claim.Token, capErr)

	var warn tickWarn
	// Chunks hold whole units, so a unit's outcome is decided in one place.
	for len(units) > 0 {
		var chunk []pushUnit
		msgs := 0
		for len(units) > 0 {
			n := min(len(units[0].devices), notify.MaxBatch)
			if len(chunk) > 0 && msgs+n > notify.MaxBatch {
				break
			}
			chunk = append(chunk, units[0])
			units = units[1:]
			msgs += n
		}
		if ctx.Err() != nil || deadline.Sub(s.pushNow()) < cfg.sendTimeout {
			s.unclaim(claim.Token, unitRowIDs(append(chunk, units...)))
			break // still emit this tick's warnings for chunks already sent
		}
		s.sendChunk(ctx, sender, claim.Token, chunk, deadline, meta, &warn)
	}
	if warn.unavailable {
		slog.Warn("push dispatch: gateway unavailable, rows released")
	}
	if warn.badRequest {
		slog.Warn("push dispatch: gateway refused a request, rows skipped")
	}
}

func unitRowIDs(units []pushUnit) []string {
	var ids []string
	for _, u := range units {
		ids = append(ids, u.rowIDs...)
	}
	return ids
}

// preferenceGates is G5/G6 and G7/G8 with their cached lookups.
func (s *Server) preferenceGates(c *pushGateCache, n store.Notification, at time.Time) (string, error) {
	set, ok := c.settings[n.UserID]
	if !ok {
		var err error
		if set, err = s.store.GetNotificationSettings(n.UserID); err != nil {
			return "", err
		}
		c.settings[n.UserID] = set
	}
	if reason := kindEnabled(set, n); reason != "" {
		return reason, nil
	}
	devs, ok := c.devices[n.UserID]
	if !ok {
		var err error
		if devs, err = s.store.LivePushDevices(n.UserID, at); err != nil {
			return "", err
		}
		c.devices[n.UserID] = devs
	}
	return deviceGate(n, devs), nil
}

func (s *Server) settleSkips(token string, skips map[string][]string) {
	for reason, ids := range skips {
		if _, err := s.store.MarkNotificationsSkipped(token, ids, reason); err != nil {
			slog.Warn("push dispatch: settle failed", "op", "skip")
		}
	}
}

func (s *Server) unclaim(token string, ids []string) {
	if len(ids) == 0 {
		return
	}
	if _, err := s.store.UnclaimNotifications(token, ids); err != nil {
		slog.Warn("push dispatch: settle failed", "op", "unclaim")
	}
}

// sendChunk sends one chunk of whole units and settles it immediately. Per
// message: sent keeps the device; invalid_token deletes it (the only result
// that does, and only while the row still holds the token that was sent);
// rejected and retry keep it. Per unit: any device sent => sent (the others are
// not chased, so nothing duplicates); otherwise any retry (or a missing
// result) => release with a backoff; otherwise every device was terminal =>
// skipped gateway_rejected if any was rejected, else no_device. A whole request
// the gateway refused (ErrBadRequest) skips every unit with no attempt; any
// other whole-request error releases them.
func (s *Server) sendChunk(ctx context.Context, sender pushSender, token string, chunk []pushUnit, deadline time.Time, meta map[string]store.Notification, warn *tickWarn) {
	// Drop rows no longer held (reclaimed, settled or deleted meanwhile).
	all := unitRowIDs(chunk)
	held, err := s.store.HeldNotifications(token, all)
	if err != nil {
		slog.Warn("push dispatch: settle failed", "op", "held")
		s.unclaim(token, all)
		return
	}
	heldSet := make(map[string]bool, len(held))
	for _, id := range held {
		heldSet[id] = true
	}
	type sentMsg struct {
		unit int
		dev  store.PushDevice
	}
	var msgs []notify.Message
	owner := map[string]sentMsg{} // message id -> unit index and device
	var live []pushUnit
	var noMsg []string // units none of whose devices could be given a message
	for _, u := range chunk {
		var ids []string
		for _, id := range u.rowIDs {
			if heldSet[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		u.rowIDs = ids
		count := 1
		if u.kind == notify.KindSummary {
			count = len(ids)
		}
		first := len(msgs)
		for _, d := range u.devices[:min(len(u.devices), notify.MaxBatch)] {
			id := notify.BatchID(len(msgs))
			m, ok := notify.NewMessage(id, d.Platform, d.Token, u.kind, u.nid, d.ConnectionID, count)
			if !ok {
				continue
			}
			owner[id] = sentMsg{len(live), d}
			msgs = append(msgs, m)
		}
		if len(msgs) == first {
			noMsg = append(noMsg, ids...)
			continue
		}
		live = append(live, u)
	}
	if len(noMsg) > 0 {
		s.settleSkips(token, map[string][]string{skipNoDevice: noMsg})
	}
	if len(msgs) == 0 {
		s.unclaim(token, unitRowIDs(live))
		return
	}

	sctx, cancel := context.WithDeadline(ctx, deadline)
	results, serr := sender.Send(sctx, msgs)
	cancel()
	if serr != nil && ctx.Err() != nil {
		s.unclaim(token, unitRowIDs(live)) // shutdown: no verdict, no attempt
		return
	}
	if errors.Is(serr, notify.ErrBadRequest) {
		warn.badRequest = true
		s.settleSkips(token, map[string][]string{skipGatewayRejected: unitRowIDs(live)})
		return
	}
	if serr != nil {
		warn.unavailable = true
		results = nil
	}
	type unitVerdict struct{ sent, retry, rejected bool }
	verdict := make([]unitVerdict, len(live))
	status := make(map[string]string, len(results))
	for _, r := range results {
		status[r.ID] = r.Status
	}
	for id, o := range owner {
		v := &verdict[o.unit]
		switch status[id] {
		case notify.StatusSent:
			v.sent = true
		case notify.StatusInvalidToken:
			if err := s.store.DeletePushDeviceIfCurrent(o.dev.ID, o.dev.Token, o.dev.RegSeq); err != nil {
				slog.Warn("push dispatch: settle failed", "op", "delete")
			}
		case notify.StatusRejected:
			v.rejected = true
		default: // retry, an unknown status, or no result at all
			v.retry = true
		}
	}
	var sentIDs, releaseIDs []string
	skips := map[string][]string{}
	for i, u := range live {
		switch v := verdict[i]; {
		case v.sent:
			sentIDs = append(sentIDs, u.rowIDs...)
		case v.retry:
			releaseIDs = append(releaseIDs, u.rowIDs...)
		case v.rejected:
			skips[skipGatewayRejected] = append(skips[skipGatewayRejected], u.rowIDs...)
		default:
			skips[skipNoDevice] = append(skips[skipNoDevice], u.rowIDs...)
		}
	}
	if len(sentIDs) > 0 {
		if _, err := s.store.RecordNotificationsSent(sentIDs); err != nil {
			// Delivered but unrecorded: park, or the row is re-sent every lease.
			// A tick claims at most notify.MaxBatch rows, inside one record
			// chunk, so the prefix is 0 today; it is honoured anyway.
			slog.Warn("push dispatch: settle failed", "op", "sent")
			s.parkUnrecorded(sentIDs[recordedPrefix(err):], meta, s.pushNow())
		}
	}
	s.settleSkips(token, skips)
	if len(releaseIDs) > 0 {
		if _, err := s.store.ReleaseNotificationsBackoff(token, releaseIDs); err != nil {
			slog.Warn("push dispatch: settle failed", "op", "release")
		}
	}
}
