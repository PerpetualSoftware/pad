package main

import (
	"context"
	"fmt"
	"log/slog"
)

// redisSlogLogger adapts go-redis's internal logging interface onto slog
// (BUG-2727).
//
// go-redis logs a handful of operational conditions through a package-level
// logger that defaults to the standard `log` package. Two of them are
// notification loss and neither has any other trace:
//
//   - "channel is full for <timeout> (message is dropped)" — a subscription's
//     buffer stayed full past the send timeout, so go-redis discarded a
//     message before Pad ever saw it. Pad detects the CONSEQUENCE as a
//     sequence gap (see watchevents.Observer), but the cause is only visible
//     here.
//   - "unknown message type" — a protocol-level surprise on the subscription.
//
// ONE LEVEL FOR A MIXED STREAM, deliberately. go-redis routes a wide mix
// through this single call — genuine failures (connection close failed,
// re-authentication failed, a handler that could not register), state
// changes, and informational fallbacks — with no severity attached and no
// structure to key on. Enumerated rather than assumed: an earlier version
// of this comment claimed the stream was mostly "benign reconnect
// chatter", which was a guess.
//
// So everything lands at WARN, and the alternative was worse in both
// directions: INFO would bury the dropped-message line this bridge exists
// for, and classifying by matching the message TEXT would make Pad's log
// levels depend on go-redis's prose — a dependency that breaks silently
// on any upstream rewording.
//
// The `component` field is how an operator makes this routable: filter or
// route on component=go-redis rather than alerting on all WARNs. The
// METRICS are what to alert on; this is what to read afterwards.
type redisSlogLogger struct{}

func (redisSlogLogger) Printf(_ context.Context, format string, v ...interface{}) {
	slog.Warn("redis: "+fmt.Sprintf(format, v...), "component", "go-redis")
}
