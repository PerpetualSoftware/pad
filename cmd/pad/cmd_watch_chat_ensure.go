package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// The server's ChatDM Retry-After is 120 seconds.
const monitorEnsureDMDefaultRetryAfter = 120 * time.Second

// The largest Retry-After, in seconds, that a time.Duration can hold
// (TASK-4043 r2 J1); a larger one would wrap negative.
const monitorEnsureDMMaxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// One worker belongs to the monitor process, across identity lookups and stream
// reconnects. Its context is the monitor root, never the floor or stream deadline.
type monitorEnsureDM struct {
	once   sync.Once
	done   <-chan struct{}
	sleep  func(context.Context, time.Duration) bool
	stderr io.Writer
}

func newMonitorEnsureDM(stderr io.Writer) *monitorEnsureDM {
	return &monitorEnsureDM{sleep: sleepOrDone, stderr: stderr}
}

func (d *monitorEnsureDM) start(ctx context.Context, agent string, ensure func(context.Context, string) error) <-chan struct{} {
	if agent == "" {
		return nil
	}
	d.once.Do(func() {
		done := make(chan struct{})
		d.done = done
		go func() {
			defer close(done)
			d.run(ctx, agent, ensure)
		}()
	})
	return d.done
}

func (d *monitorEnsureDM) run(ctx context.Context, agent string, ensure func(context.Context, string) error) {
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			return
		}
		err := ensure(ctx, agent)
		if err == nil || ctx.Err() != nil {
			return
		}
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests && attempt < 3 {
			// No usable header (0 or negative) or one too large for a Duration
			// waits the default, so no input can make the retry hot.
			retryAfter := monitorEnsureDMDefaultRetryAfter
			if secs := int64(apiErr.RetryAfterSeconds); secs > 0 && secs <= monitorEnsureDMMaxRetryAfterSeconds {
				retryAfter = time.Duration(secs) * time.Second
			}
			if !d.sleep(ctx, retryAfter) {
				return
			}
			continue
		}
		// The error can contain newlines; every failed outcome is one line.
		fmt.Fprintf(d.stderr, "Pad chat: could not ensure owner-agent DM: %s\n", strings.Join(strings.Fields(err.Error()), " "))
		return
	}
}
