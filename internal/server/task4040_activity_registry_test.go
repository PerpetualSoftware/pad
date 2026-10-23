package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

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
				got, err := reader.IsBusy(user, worker)
				if err != nil || got != want {
					t.Fatalf("IsBusy(%s,%s) = %v,%v, want %v", user, worker, got, err, want)
				}
			}
			if err := writer.SetActivity("B", presenceWorkerA, true, now); err != nil {
				t.Fatal(err)
			}
			assertBusy("A", presenceWorkerA, false)
			if err := writer.SetActivity("A", presenceWorkerA, true, now); err != nil {
				t.Fatal(err)
			}
			assertBusy("A", presenceWorkerA, true)
			assertBusy("A", presenceWorkerB, false)
			assertBusy("A", "", false)
			writer.Remove("A", first)
			writer.Add("A", ident, SessionOrigin{})
			assertBusy("A", presenceWorkerA, true) // Across reconnect or /clear.
			if err := writer.SetActivity("A", presenceWorkerA, false, now); err != nil {
				t.Fatal(err)
			}
			assertBusy("A", presenceWorkerA, false)
			if err := writer.SetActivity("A", presenceWorkerA, true, now); err != nil {
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
			if err := writer.SetActivity(user.ID, presenceWorkerA, true, now); err != nil {
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
	if err := p.SetActivity("A", presenceWorkerA, true, now); err != nil {
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
	if busy, err := other.IsBusy("A", presenceWorkerA); err != nil || busy {
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
	if busy, err := p.IsBusy("A", presenceWorkerA); err != nil || !busy {
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
