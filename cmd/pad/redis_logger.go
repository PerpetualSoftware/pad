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
// Logged at WARN rather than ERROR: none of them is actionable on its own,
// and go-redis also logs benign reconnect chatter through the same call.
// The metrics are what to alert on; this is what to read afterwards.
type redisSlogLogger struct{}

func (redisSlogLogger) Printf(_ context.Context, format string, v ...interface{}) {
	slog.Warn("redis: " + fmt.Sprintf(format, v...))
}
