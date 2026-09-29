package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// TASK-3298: stdin EOF must not cancel requests that are already in flight.
//
// `pad mcp serve` is also driven non-interactively: a script pipes a batch of
// JSON-RPC requests in and closes stdin, expecting one answer per id. mcp-go
// v1.1.1 changed StdioServer.Listen to cancel every in-flight request's
// context the moment it reads EOF (upstream #977), so each dispatcher-backed
// call answered `server_error: context canceled` instead of its result. That
// release is excluded in .github/dependabot.yml; this test is the pin that
// keeps any later mcp-go version from bringing the behaviour back unnoticed.
//
// Ordering is by observation, not by sleeping. Three tools/call (mcp-go's
// worker pool, default size 5) and one resources/read are held inside the
// dispatcher and the fetcher; stdin is closed only once all four are inside.
// The tool calls are released only AFTER the server's own read of stdin has
// returned EOF, so on any version the EOF is processed while they are still
// in flight. The resource read is released on the same signal, or after
// resourceFallback when the server serves it on the read loop itself (as
// v1.1.0 does) and therefore cannot read EOF until it returns; in that shape
// EOF cannot cancel it, and the tool calls are still held across the EOF.
//
// Two bounds, stated rather than implied. (1) The signal fires inside Read,
// before the server has handled the EOF, and a server that never cancels
// emits nothing to wait for, so the release after it is a timed settle: a
// cancel-at-EOF server whose EOF handling is delayed past eofSettle would
// pass. That error runs one way only; a server that keeps in-flight calls
// alive can never be failed by it. (2) A server that runs tool calls on the
// read loop itself lets only one call in before stdin closes, so the entry
// gate fails with "only 1 of 4 calls reached"; that means this pin's premise
// no longer holds, not that EOF cancelled anything.

const (
	// eofSettle is how long a release waits after EOF was observed, so a
	// cancel-at-EOF server has cancelled before the release can win the race.
	eofSettle = 500 * time.Millisecond
	// resourceFallback releases a resource read that blocks the read loop.
	resourceFallback = time.Second
)

// eofSignalReader closes seen the first time the wrapped reader returns
// io.EOF, i.e. when the server's read loop has actually reached EOF.
type eofSignalReader struct {
	r    io.Reader
	once sync.Once
	seen chan struct{}
}

func (e *eofSignalReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		e.once.Do(func() { close(e.seen) })
	}
	return n, err
}

// blockingDispatcher holds each call until release is closed, and reports
// the context's error instead if the context ends first.
type blockingDispatcher struct {
	entered chan struct{}
	release chan struct{}
}

func (d *blockingDispatcher) Dispatch(ctx context.Context, _ []string, _ []string) (*mcp.CallToolResult, error) {
	d.entered <- struct{}{}
	select {
	case <-d.release:
		return mcp.NewToolResultText("dispatched"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type blockingFetcher struct {
	entered chan struct{}
	release chan struct{}
}

func (f *blockingFetcher) Fetch(ctx context.Context, _ []string) (string, error) {
	f.entered <- struct{}{}
	select {
	case <-f.release:
		return `[{"slug":"docapp","name":"Pad"}]`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestStdio_EOFDoesNotCancelInFlightRequests(t *testing.T) {
	const toolCalls = 3
	entered := make(chan struct{}, toolCalls+1)
	toolRelease := make(chan struct{})
	resourceRelease := make(chan struct{})
	var releaseTools, releaseResource sync.Once
	freeTools := func() { releaseTools.Do(func() { close(toolRelease) }) }
	freeResource := func() { releaseResource.Do(func() { close(resourceRelease) }) }

	srv := NewServer(Options{Version: "eof-test"})
	if _, err := RegisterCatalog(srv.MCP(), CatalogOptions{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: &blockingDispatcher{entered: entered, release: toolRelease},
		PadVersion: "test",
	}); err != nil {
		t.Fatalf("RegisterCatalog: %v", err)
	}
	RegisterResources(srv.MCP(), &blockingFetcher{entered: entered, release: resourceRelease}, nil)

	pipeR, stdinW := io.Pipe()
	stdin := &eofSignalReader{r: pipeR, seen: make(chan struct{})}
	stdoutR, stdout := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- srv.RunStdio(context.Background(), stdin, stdout)
		_ = stdout.Close()
	}()
	// On any failure path, unblock every held call and both pipes so no
	// goroutine outlives the test.
	t.Cleanup(func() {
		freeTools()
		freeResource()
		_ = stdinW.Close()
		_ = stdoutR.Close()
	})

	// Collect every response line until the server closes stdout.
	type response struct {
		ID     int              `json:"id"`
		Error  *json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
		raw string
	}
	responses := make(chan []response, 1)
	go func() {
		var got []response
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var r response
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				r.raw = sc.Text()
				got = append(got, r)
			}
		}
		responses <- got
	}()

	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"eof-test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}
	for i := 0; i < toolCalls; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"pad_item","arguments":{"action":"get","ref":"TASK-%d"}}}`,
			10+i, i+1))
	}
	// Last, because a server that serves it on the read loop reads nothing
	// after it until it returns.
	lines = append(lines, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":20,"method":"resources/read","params":{"uri":%q}}`, WorkspacesURI))

	go func() {
		_, _ = io.WriteString(stdinW, strings.Join(lines, "\n")+"\n")
	}()

	// Every call is inside the dispatcher or fetcher before stdin closes.
	for i := 0; i < toolCalls+1; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d calls reached the dispatcher/fetcher", i, toolCalls+1)
		}
	}
	_ = stdinW.Close()

	select {
	case <-stdin.seen:
		// Off-loop resource read: EOF arrived with everything still held.
	case <-time.After(resourceFallback):
		// On-loop resource read: it must return before EOF can be read.
		freeResource()
		select {
		case <-stdin.seen:
		case <-time.After(5 * time.Second):
			t.Fatal("server never read stdin EOF")
		}
	}
	time.Sleep(eofSettle)
	freeResource()
	freeTools()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunStdio: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunStdio did not return after stdin EOF")
	}
	got := <-responses

	byID := map[int]response{}
	for _, r := range got {
		byID[r.ID] = r
	}
	want := []int{1, 20}
	for i := 0; i < toolCalls; i++ {
		want = append(want, 10+i)
	}
	for _, id := range want {
		r, ok := byID[id]
		if !ok {
			t.Errorf("id %d: no response after stdin EOF", id)
			continue
		}
		if r.Error != nil || r.Result.IsError {
			t.Errorf("id %d: answered with an error after stdin EOF: %s", id, r.raw)
		}
	}
}
