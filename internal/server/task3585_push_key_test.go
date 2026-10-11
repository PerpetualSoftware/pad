package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TASK-3585: idempotency keys on push. Push writes no row, so the key is
// completed after the publish; every publish carries a push_id derived from
// the key so a monitor can drop a re-publish after a claim takeover.

// capturingBus records what was published and fails with err when set.
type capturingBus struct {
	mu   sync.Mutex
	err  error
	sent []watchevents.Notification
}

func (b *capturingBus) Publish(n watchevents.Notification) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, n)
	return b.err
}
func (b *capturingBus) published() []watchevents.Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]watchevents.Notification(nil), b.sent...)
}
func (b *capturingBus) setErr(err error) { b.mu.Lock(); b.err = err; b.mu.Unlock() }
func (b *capturingBus) Subscribe() (chan watchevents.Notification, <-chan struct{}, error) {
	return nil, nil, nil
}
func (b *capturingBus) SubscribeAndReplaySince(context.Context, int64) (chan watchevents.Notification, []watchevents.Notification, <-chan struct{}, error) {
	return nil, nil, nil, nil
}
func (b *capturingBus) Unsubscribe(chan watchevents.Notification) {}
func (b *capturingBus) EventsSince(int64) []watchevents.Notification {
	return nil
}
func (b *capturingBus) Close() {}

func pushOnce(t *testing.T, srv *Server, slug, itemSlug, token, key string) *http.Response {
	t.Helper()
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemSlug+"/push", token,
		map[string]interface{}{"message": "triage this", "op_key": key})
	return rr.Result()
}

func TestTASK3585_PushReplaysWithoutRedelivering(t *testing.T) {
	srv := testServer(t)
	bus := &capturingBus{}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	first := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-1")
	again := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-1")
	if first.StatusCode != http.StatusOK || again.StatusCode != http.StatusOK || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d/%d", first.StatusCode, again.StatusCode)
	}
	if n := len(bus.published()); n != 1 {
		t.Fatalf("a same-key retry published again: %d publishes", n)
	}
	if bus.published()[0].PushID == "" {
		t.Fatal("a push must carry a push_id")
	}
}

func TestTASK3585_AmbiguousPushIsNotRedelivered(t *testing.T) {
	srv := testServer(t)
	bus := &capturingBus{err: errors.New("redis: i/o timeout")}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	first := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-2")
	if first.StatusCode != http.StatusBadGateway {
		t.Fatalf("premise: an unconfirmed publish answers 502; got %d", first.StatusCode)
	}
	bus.setErr(nil)
	again := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-2")
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(again.Body).Decode(&env)
	if again.StatusCode != http.StatusConflict || env.Error.Code != pushOutcomeUnknownCode {
		t.Fatalf("retry after an ambiguous publish: %d %q", again.StatusCode, env.Error.Code)
	}
	if n := len(bus.published()); n != 1 {
		t.Fatalf("the ambiguous push was delivered again: %d publishes", n)
	}
}

func TestTASK3585_ClosedBusReleasesThePushKey(t *testing.T) {
	srv := testServer(t)
	bus := &capturingBus{err: watchevents.ErrBusClosed}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	if r := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-3"); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("premise: a closed bus answers 503; got %d", r.StatusCode)
	}
	bus.setErr(nil)
	again := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-3")
	if again.StatusCode != http.StatusOK || again.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("a push that published nothing must be sendable again with its key: %d", again.StatusCode)
	}
}

// A server that dies between publishing and completing the key leaves a
// claim whose lease expires. The retry takes it over and publishes again,
// with the SAME push_id, which is what lets a monitor drop the repeat.
func TestTASK3585_PushTakeoverKeepsThePushID(t *testing.T) {
	srv := testServer(t)
	bus := &capturingBus{}
	srv.SetWatchEventsBus(bus)
	slug, item, tok, user := setupWatchTestUser(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	hash := idemRequestHash(idempotencyRoutePush, item.ID, pushRequest{Message: "triage this"})
	// The dead first attempt: claimed, never completed, lease already over.
	if _, err := srv.store.ClaimIdempotencyKey(ws.ID, user.ID, "p-4", idempotencyRoutePush, hash, -time.Second); err != nil {
		t.Fatal(err)
	}
	r := pushOnce(t, srv, slug, item.Slug, tok.Token, "p-4")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("takeover: %d", r.StatusCode)
	}
	sent := bus.published()
	if len(sent) != 1 || sent[0].PushID != pushIDFor(ws.ID, user.ID, "p-4") {
		t.Fatalf("the takeover must publish the key's stable push_id: %+v", sent)
	}
}
