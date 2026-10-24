package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Record the activity commands separately from ListForUser's own Redis reads.
type activityReadHook struct {
	mu    sync.Mutex
	calls [][]string
}

func (h *activityReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *activityReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *activityReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" || cmd.Name() == "mget" {
			var keys []string
			for _, arg := range cmd.Args()[1:] {
				if key, ok := arg.(string); ok && strings.Contains(key, "activity:") {
					keys = append(keys, key)
				}
			}
			if len(keys) > 0 {
				h.mu.Lock()
				h.calls = append(h.calls, append([]string{cmd.Name()}, keys...))
				h.mu.Unlock()
			}
		}
		return next(ctx, cmd)
	}
}

func TestListSessions_ActivitySingleMGet(t *testing.T) {
	writer, reader, mr := newRedisPresencePair(t)
	srv := testServerWithPresence(t)
	srv.SetSessionPresence(reader)
	_, _, tok, user := setupWatchTestUser(t, srv)
	if err := writer.SetActivity(user.ID, presenceWorkerA, true, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	mr.Set(writer.activityKey(user.ID, presenceWorkerB), "garbage activity")
	want := map[string]string{
		writer.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerA, Armed: true}, SessionOrigin{}): "busy",
		writer.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerA}, SessionOrigin{}):              "busy",
		writer.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerB, Armed: true}, SessionOrigin{}): "online",
	}
	hook := &activityReadHook{}
	reader.client.AddHook(hook)
	rows := presenceRows(t, srv, tok.Token) // Also asserts HTTP 200.
	if len(rows) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if expected, ok := want[row["id"].(string)]; !ok || row["state"] != expected {
			t.Errorf("unexpected session state: %+v (want %s)", row, expected)
		}
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if len(hook.calls) != 1 || hook.calls[0][0] != "mget" || len(hook.calls[0]) != 3 {
		t.Fatalf("want one MGET with two worker keys, got %v", hook.calls)
	}
	keys := map[string]bool{hook.calls[0][1]: true, hook.calls[0][2]: true}
	if !keys[writer.activityKey(user.ID, presenceWorkerA)] || !keys[writer.activityKey(user.ID, presenceWorkerB)] {
		t.Fatalf("unexpected MGET keys: %v", hook.calls)
	}
}

type failedActivityPresence struct {
	MemorySessionPresence
	reads int
}

func (p *failedActivityPresence) BusyWorkers(string, []string) (map[string]bool, error) {
	p.reads++
	return nil, errors.New("activity batch unavailable")
}

func TestListSessions_ActivityBatchFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	p := &failedActivityPresence{MemorySessionPresence: *NewMemorySessionPresence()}
	srv := testServerWithPresence(t)
	srv.SetSessionPresence(p)
	_, _, tok, user := setupWatchTestUser(t, srv)
	for _, worker := range []string{presenceWorkerA, presenceWorkerB} {
		if err := p.SetActivity(user.ID, worker, true, time.Now(), nil); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		p.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerA, Armed: true}, SessionOrigin{}): "online",
		p.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerA}, SessionOrigin{}):              "connected_not_accepting",
		p.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerB}, SessionOrigin{}):              "connected_not_accepting",
	}
	rows := presenceRows(t, srv, tok.Token)
	if len(rows) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if expected, ok := want[row["id"].(string)]; !ok || row["state"] != expected {
			t.Errorf("unexpected session state: %+v (want %s)", row, expected)
		}
	}
	if p.reads != 1 {
		t.Errorf("got %d activity batches, want one", p.reads)
	}
	if got := bytes.Count(logs.Bytes(), []byte("sessions: activity read failed")); got != 1 {
		t.Fatalf("got %d warnings, want one: %s", got, &logs)
	}
}

func TestListSessions_CorruptActivityDegrades(t *testing.T) {
	// Do not run in parallel: capture the handler's request-level warning.
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	writer, reader, mr := newRedisPresencePair(t)
	srv := testServerWithPresence(t)
	srv.SetSessionPresence(reader)
	_, _, tok, user := setupWatchTestUser(t, srv)
	corruptWorker := "wk_cccccccccccccccccccccccccccccccc"
	for _, worker := range []string{presenceWorkerA, corruptWorker} {
		mr.Set(writer.activityKey(user.ID, worker), "garbage activity")
	}
	if err := writer.SetActivity(user.ID, presenceWorkerB, true, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		writer.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerA, Armed: true}, SessionOrigin{}): "online",
		writer.Add(user.ID, SessionIdentity{WorkerID: corruptWorker}, SessionOrigin{}):                "connected_not_accepting",
		writer.Add(user.ID, SessionIdentity{WorkerID: presenceWorkerB, Armed: true}, SessionOrigin{}): "busy",
		writer.Add(user.ID, SessionIdentity{Armed: true}, SessionOrigin{}):                            "online",
	}
	// Two requests prove that warnings are limited per request, not globally.
	for request := 1; request <= 2; request++ {
		rows := presenceRows(t, srv, tok.Token) // Asserts HTTP 200.
		if len(rows) != len(want) {
			t.Fatalf("got %d sessions, want %d", len(rows), len(want))
		}
		for _, row := range rows {
			if expected, ok := want[row["id"].(string)]; !ok || row["state"] != expected {
				t.Errorf("unexpected session state: %+v (want %s)", row, expected)
			}
		}
		if got := bytes.Count(logs.Bytes(), []byte("sessions: activity read failed")); got != request {
			t.Fatalf("got %d warnings after %d requests, want one per request: %s", got, request, &logs)
		}
	}
}

func TestPresenceActivityRegistries(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			now := time.Now().UTC()
			var writer, reader SessionPresence
			var advance func(time.Duration)
			if backend == "memory" {
				p := NewMemorySessionPresence()
				p.now = func() time.Time { return now }
				writer, reader = p, p
				advance = func(d time.Duration) { now = now.Add(d) }
			} else {
				p, q, mr := newRedisPresencePair(t)
				p.now = func() time.Time { return now }
				q.now = func() time.Time { return now }
				writer, reader = p, q
				advance = func(d time.Duration) { now = now.Add(d); mr.FastForward(d) }
			}
			ident := SessionIdentity{Agent: "implementer", WorkerID: presenceWorkerA, Armed: true}
			first := writer.Add("A", ident, SessionOrigin{})
			rows := mustList(t, reader, "A")
			if len(rows) != 1 || rows[0].Agent != ident.Agent || rows[0].WorkerID != ident.WorkerID {
				t.Fatalf("identity: %+v", rows)
			}
			assertBusy := func(user, worker string, want bool) {
				t.Helper()
				got, err := reader.BusyWorkers(user, []string{worker, worker, ""})
				if err != nil || got[worker] != want {
					t.Fatalf("BusyWorkers(%s,%s) = %v,%v, want %v", user, worker, got, err, want)
				}
			}
			if err := writer.SetActivity("B", presenceWorkerA, true, now, nil); err != nil {
				t.Fatal(err)
			}
			assertBusy("A", presenceWorkerA, false)
			if err := writer.SetActivity("A", presenceWorkerA, true, now, nil); err != nil {
				t.Fatal(err)
			}
			assertBusy("A", presenceWorkerA, true)
			assertBusy("A", presenceWorkerB, false)
			assertBusy("A", "", false)
			writer.Remove("A", first)
			writer.Add("A", ident, SessionOrigin{})
			assertBusy("A", presenceWorkerA, true) // Across reconnect or /clear.
			if err := writer.SetActivity("A", presenceWorkerA, false, now, nil); err != nil {
				t.Fatal(err)
			}
			assertBusy("A", presenceWorkerA, false)
			if err := writer.SetActivity("A", presenceWorkerA, true, now, nil); err != nil {
				t.Fatal(err)
			}
			advance(30 * time.Minute)
			assertBusy("A", presenceWorkerA, false)
			// Reconnect after expiry; an armed session reports online, then busy,
			// then online again after advancing the registry clock without sleeping.
			writer.Add("A", ident, SessionOrigin{})
			srv := testServerWithPresence(t)
			srv.SetSessionPresence(reader)
			_, _, tok, user := setupWatchTestUser(t, srv)
			writer.Add(user.ID, ident, SessionOrigin{})
			if err := writer.SetActivity(user.ID, presenceWorkerA, true, now, nil); err != nil {
				t.Fatal(err)
			}
			if got := presenceRows(t, srv, tok.Token)[0]["state"]; got != "busy" {
				t.Fatalf("state: %v", got)
			}
			advance(30 * time.Minute)
			// Redis session TTL also elapsed: add a replacement with the same worker.
			writer.Add(user.ID, ident, SessionOrigin{})
			for _, row := range presenceRows(t, srv, tok.Token) {
				if row["state"] != "online" {
					t.Fatalf("expired busy: %+v", row)
				}
			}
		})
	}
}

func TestPresenceActivityRedisNamespaceAndTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	keys, err := redisns.Parse("presence-4040")
	if err != nil {
		t.Fatal(err)
	}
	p := NewRedisSessionPresenceWithKeys(client, keys)
	t.Cleanup(p.Close)
	now := time.Now()
	p.now = func() time.Time { return now }
	if err := p.SetActivity("A", presenceWorkerA, true, now, nil); err != nil {
		t.Fatal(err)
	}
	key := keys.Name("activity:A:" + presenceWorkerA)
	if !mr.Exists(key) {
		t.Fatalf("missing namespaced key: %v", mr.Keys())
	}
	if ttl := mr.TTL(key); ttl != 30*time.Minute {
		t.Fatalf("TTL = %v", ttl)
	}
	other := NewRedisSessionPresence(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	t.Cleanup(other.Close)
	if busy, err := other.BusyWorkers("A", []string{presenceWorkerA}); err != nil || busy[presenceWorkerA] {
		t.Fatalf("namespace leak: %v %v", busy, err)
	}
	// Activity lives outside frozen session JSON and survives a renewal write.
	id := p.Add("A", SessionIdentity{WorkerID: presenceWorkerA}, SessionOrigin{})
	payload, err := client.Get(t.Context(), p.sessionKey("A", id)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.write(t.Context(), "A", id, payload); err != nil {
		t.Fatal(err)
	}
	if busy, err := p.BusyWorkers("A", []string{presenceWorkerA}); err != nil || !busy[presenceWorkerA] {
		t.Fatalf("renewal lost busy: %v %v", busy, err)
	}
	mr.FastForward(30 * time.Minute)
	if mr.Exists(key) {
		t.Fatal("activity did not expire")
	}
}

func TestSessionActivityRequiresResolvedUser(t *testing.T) {
	srv := testServerWithPresence(t) // Fresh install: no principal even when auth is bypassed.
	rr := presenceRequest(t, srv, http.MethodPost, "", presenceWorkerA, `{"state":"busy"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}
