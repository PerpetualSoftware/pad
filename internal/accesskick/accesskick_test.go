package accesskick

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
	got := make(chan Message, 1)
	stop := b.Subscribe(func(m Message) { got <- m })
	defer stop()
	if err := a.Publish(context.Background(), Message{UserID: "u1", WorkspaceID: "w1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.UserID != "u1" || m.WorkspaceID != "w1" {
			t.Fatalf("got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
	}
}

// codex r2: the shared client ignores a context deadline on command I/O, so a
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
	start := time.Now()
	err = tr.Publish(context.Background(), Message{UserID: "u1"})
	if err == nil {
		t.Fatal("publish to a server that never answers succeeded")
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("publish took %v; the 1s bound was not honoured", d)
	}
}
