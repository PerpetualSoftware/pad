package server

import (
	"context"
	"errors"
	"log/slog"
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
// the rows. B1 is the decide-and-send core; StartPushDispatch is NOT called
// from cmd/pad yet.
//
// LEASE INVARIANT (r6-Q1): lease >= sendTimeout + leaseMargin. A claim is
// stamped with second precision (up to 1s behind the real clock), and a Send
// can take sendTimeout (notify.SendTimeout), so a lease shorter than that lets
// another instance reclaim a row mid-send and push it twice. It is enforced
// three ways: the setter refuses a violating config, all gateway I/O runs
// under a deadline of start+lease-leaseMargin, and a chunk only starts when
// sendTimeout is still left before that deadline (otherwise its rows are
// unclaimed, no attempt burned).
//
// Logging: fixed strings and counts only. A store error can carry a row id or
// a value, so it is never logged here.
const (
	defaultPushInterval    = 5 * time.Second
	defaultPushSettle      = 10 * time.Second
	defaultPushLease       = 60 * time.Second
	defaultPushLeaseMargin = 15 * time.Second
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
	instanceID  string
	// tick, when non-nil, replaces the interval ticker so a test drives passes.
	tick <-chan time.Time
}

type pushDispatchSettings struct {
	interval, settle, lease, leaseMargin, sendTimeout time.Duration
	instanceID                                        string
}

// SetPushDispatchConfig overrides the timings; zero keeps the current value.
// A combination violating the lease invariant is refused whole and nothing
// changes.
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
	if next.lease < next.sendTimeout+next.leaseMargin {
		slog.Warn("push dispatch: config refused", "reason", "lease shorter than send timeout plus margin")
		return errPushLeaseTooShort
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
	kind    string
	devices []store.PushDevice
}

// kindPriority breaks a created_at tie between rows of one group, so the kind
// of a mixed group is deterministic (ids are random).
var kindPriority = map[string]int{
	notify.KindReminder: 0, notify.KindComment: 1, notify.KindAssigned: 2, notify.KindReply: 3, notify.KindMention: 4,
}

// runPushDispatchTick is one pass. ctx is the shutdown signal.
func (s *Server) runPushDispatchTick(ctx context.Context) {
	cfg := s.resolvePushDispatchSettings()
	sender := s.pushSenderOrNil()
	if sender == nil {
		return
	}
	start := s.pushNow()
	// Fresh per tick and never kept: the store appends its own nonce, and
	// every settle below uses Claim.Token.
	token := "pd-" + cfg.instanceID + "-" + uuid.NewString()
	claim, err := s.store.ClaimPendingNotifications(token, notify.MaxBatch, start.Add(-cfg.settle), start.Add(-cfg.lease))
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
	for _, n := range claim.Rows {
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

	var units []pushUnit
	for _, key := range order {
		rows := groups[key]
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].CreatedAt != rows[j].CreatedAt {
				return rows[i].CreatedAt < rows[j].CreatedAt
			}
			if pi, pj := kindPriority[rows[i].Kind], kindPriority[rows[j].Kind]; pi != pj {
				return pi < pj
			}
			return rows[i].ID < rows[j].ID
		})
		newest := rows[len(rows)-1]
		u := pushUnit{nid: newest.ID, kind: newest.Kind, devices: cache.devices[newest.UserID]}
		for _, r := range rows {
			u.rowIDs = append(u.rowIDs, r.ID)
		}
		units = append(units, u)
	}

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
			return
		}
		s.sendChunk(ctx, sender, claim.Token, chunk, deadline)
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

// sendChunk sends one chunk of whole units and settles it immediately. B1: a
// unit any of whose devices was sent is sent; every other outcome releases
// its rows (attempts + 1).
func (s *Server) sendChunk(ctx context.Context, sender pushSender, token string, chunk []pushUnit, deadline time.Time) {
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
	var msgs []notify.Message
	owner := map[string]int{} // message id -> unit index
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
		first := len(msgs)
		for _, d := range u.devices[:min(len(u.devices), notify.MaxBatch)] {
			id := notify.BatchID(len(msgs))
			m, ok := notify.NewMessage(id, d.Platform, d.Token, u.kind, u.nid, d.ConnectionID, 1)
			if !ok {
				continue
			}
			owner[id] = len(live)
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
	sent := make([]bool, len(live))
	if serr == nil {
		for _, r := range results {
			if i, ok := owner[r.ID]; ok && r.Status == notify.StatusSent {
				sent[i] = true
			}
		}
	}
	var sentIDs, releaseIDs []string
	for i, u := range live {
		if sent[i] {
			sentIDs = append(sentIDs, u.rowIDs...)
		} else {
			releaseIDs = append(releaseIDs, u.rowIDs...)
		}
	}
	if len(sentIDs) > 0 {
		if _, err := s.store.RecordNotificationsSent(sentIDs); err != nil {
			slog.Warn("push dispatch: settle failed", "op", "sent")
		}
	}
	if len(releaseIDs) > 0 {
		if _, err := s.store.ReleaseNotifications(token, releaseIDs); err != nil {
			slog.Warn("push dispatch: settle failed", "op", "release")
		}
	}
}
