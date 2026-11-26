package homehint

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

func TestRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	shared := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer shared.Close()
	a, b := NewRedisTransport(shared, redisns.Default), NewRedisTransport(shared, redisns.Default)
	defer a.Close()
	defer b.Close()
	got := make(chan Message, 1)
	stop := b.Subscribe(func(m Message) { got <- m })
	defer stop()
	want := Message{
		Origin:       "origin-a",
		WorkspaceID:  "w1",
		CollectionID: "c1",
		ItemID:       "i1",
		Timestamp:    1700000000123,
	}
	if err := a.Publish(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != want {
			t.Fatalf("got %+v, want %+v", m, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
	}
}

// The shared client ignores a context deadline on command I/O, so a
// stalled Redis held a publish for its full read timeout. The transport's own
// client honours the 1s bound.
func TestPublishIsBoundedAgainstAStalledServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept, read, never answer
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	shared := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), ReadTimeout: 5 * time.Second})
	defer shared.Close()
	tr := NewRedisTransport(shared, redisns.Default)
	defer tr.Close()
	start := time.Now()
	err = tr.Publish(context.Background(), Message{Origin: "o1", WorkspaceID: "w1", Timestamp: 1})
	if err == nil {
		t.Fatal("publish to a server that never answers succeeded")
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("publish took %v; the 1s bound was not honoured", d)
	}
}

// The clone gets its own push-notification processor rather than
// the source client's.
func TestCloneDoesNotShareTheNotificationProcessor(t *testing.T) {
	mr := miniredis.RunT(t)
	shared := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer shared.Close()
	tr := NewRedisTransport(shared, redisns.Default)
	defer tr.Close()
	if p := shared.Options().PushNotificationProcessor; p != nil && p == tr.client.Options().PushNotificationProcessor {
		t.Fatal("the transport's client shares the source client's push-notification processor")
	}
}
