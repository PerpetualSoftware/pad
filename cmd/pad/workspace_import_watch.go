package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-3476: a bundle import used to print nothing until the server answered,
// sit silent on a dead TCP path until the kernel gave up (~15 min) or the 1h
// client cap fired, and, when the answer was lost, leave the user unable to
// tell whether the workspace exists, so a re-run could make a duplicate.
//
// importWatch runs the upload and watches it:
//   - progress to stderr while bytes go out;
//   - a stall (the transport takes no bytes for stallAfter) abandons it;
//   - once every byte is out, it polls the attempt's import key, and a
//     settled outcome seen twice while the answer still has not arrived ends
//     the wait (the answer was lost on the way back);
//   - after any failure that is not the server's own answer, it asks the
//     key once and reports what the server knows, or that nobody knows.
type importWatch struct {
	total  int64
	body   *cli.CountingReader
	stderr io.Writer
	tty    bool
	tick   time.Duration // progress line cadence
	// stallAfter: the transport took no bytes for this long. A read means the
	// transport took bytes, not that they arrived, so this measures the
	// connection's progress, which is what blocks on a dead path. The CLI
	// uses 90s, past the server's own 60s per-Read deadline: by then the
	// server has stopped waiting, so going on cannot succeed. The clock
	// starts before the connection, so a dial that hangs counts too. An
	// abandoned attempt is resolved through its key, never guessed.
	stallAfter time.Duration
	pollEvery  time.Duration // import-status cadence after the upload
	settleWait time.Duration // how long to wait for a RUNNING attempt to settle after a failure
	upload     func(ctx context.Context) (http.Header, error)
	status     func() (*cli.ImportStatus, error)

	lineOpen bool // a TTY progress line is waiting for its newline
}

// errUploadStalled and errAnswerLost are the watcher's own reasons for
// abandoning the request, distinguishable from a transport failure.
var (
	errUploadStalled = errors.New("upload stalled")
	errAnswerLost    = errors.New("the server finished but its answer did not arrive")
)

// importResolved is an import whose request failed but whose outcome the
// server reported as a created workspace: the caller prints success.
type importResolved struct {
	status *cli.ImportStatus
}

func (w *importWatch) run() (http.Header, *importResolved, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	type result struct {
		header http.Header
		err    error
	}
	done := make(chan result, 1)
	go func() {
		h, err := w.upload(ctx)
		done <- result{h, err}
	}()

	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	var lastPoll time.Time
	uploaded, settledSeen := false, 0
	for {
		select {
		case res := <-done:
			w.endLine()
			if res.err == nil {
				return res.header, nil, nil
			}
			return w.afterFailure(res.err, context.Cause(ctx))
		case <-ticker.C:
			sent := w.body.Sent()
			if !uploaded && sent < w.total {
				w.progress(sent)
				if since := w.body.SinceLastRead(); since >= w.stallAfter {
					cancel(errUploadStalled)
				}
				continue
			}
			if !uploaded {
				uploaded = true
				w.progress(sent)
				w.endLine()
				fmt.Fprintln(w.stderr, "Upload complete; the server is importing.")
			}
			if time.Since(lastPoll) < w.pollEvery {
				continue
			}
			lastPoll = time.Now()
			if st, err := w.status(); err == nil && st.State != cli.ImportStateRunning {
				settledSeen++
				if settledSeen >= 2 {
					cancel(errAnswerLost)
				}
			} else {
				settledSeen = 0
			}
		}
	}
}

// afterFailure says what a failed import request left behind. The server's
// own answer (a JSON error envelope) is the outcome and is returned as is;
// anything else means the answer did not arrive, so it asks the import key.
func (w *importWatch) afterFailure(err, cause error) (http.Header, *importResolved, error) {
	var apiErr *cli.APIError
	if errors.As(err, &apiErr) && !errors.Is(cause, errAnswerLost) && !errors.Is(cause, errUploadStalled) {
		return nil, nil, err
	}
	why := err.Error()
	switch {
	case errors.Is(cause, errUploadStalled):
		why = fmt.Sprintf("the upload stalled: the connection took no more of the bundle for %s", w.stallAfter)
	case errors.Is(cause, errAnswerLost):
		why = errAnswerLost.Error()
	}

	st, serr := w.status()
	deadline := time.Now().Add(w.settleWait)
	for serr == nil && st.State == cli.ImportStateRunning && time.Now().Before(deadline) {
		time.Sleep(w.pollEvery)
		st, serr = w.status()
	}
	if serr != nil {
		return nil, nil, fmt.Errorf("%s, and the outcome is unknown (%v). Check 'pad workspace list' before re-running: a re-run may create a duplicate", why, serr)
	}
	switch st.State {
	case cli.ImportStateComplete:
		return nil, &importResolved{status: st}, nil
	case cli.ImportStateKept:
		return nil, nil, fmt.Errorf("%s. The import stopped partway and the partial workspace %q (slug: %s) was kept: inspect or delete it before re-running", why, st.WorkspaceName, st.WorkspaceSlug)
	case cli.ImportStateRemoved:
		return nil, nil, fmt.Errorf("%s. The server removed the workspace it had started, so nothing was created: it is safe to re-run", why)
	case cli.ImportStateNotCreated:
		return nil, nil, fmt.Errorf("%s. The import failed before any workspace was created: it is safe to re-run", why)
	default:
		return nil, nil, fmt.Errorf("%s, and the server could not establish the outcome (state %q). Check 'pad workspace list' before re-running: a re-run may create a duplicate", why, st.State)
	}
}

func (w *importWatch) progress(sent int64) {
	pct := 100.0
	if w.total > 0 {
		pct = float64(sent) * 100 / float64(w.total)
	}
	line := fmt.Sprintf("Uploading: %s / %s (%.0f%%)", humanBytes(sent), humanBytes(w.total), pct)
	if w.tty {
		fmt.Fprintf(w.stderr, "\r%s", line+strings.Repeat(" ", 8))
		w.lineOpen = true
		return
	}
	fmt.Fprintln(w.stderr, line)
}

// endLine finishes an in-place progress line so the next output starts clean.
func (w *importWatch) endLine() {
	if w.lineOpen {
		fmt.Fprintln(w.stderr)
		w.lineOpen = false
	}
}
