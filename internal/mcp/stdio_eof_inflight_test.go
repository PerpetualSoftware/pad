package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
// The calls are held inside the dispatcher and the fetcher until stdin has
// been closed, so EOF is guaranteed to arrive while all of them are in
// flight: three tools/call (served by mcp-go's worker pool, default size 5)
// and one resources/read, sent last because v1.1.0 serves non-tool requests
// on the read loop itself. They are released only after EOF has had time to
// be read, and every id must then answer successfully.

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
	release := make(chan struct{})

	srv := NewServer(Options{Version: "eof-test"})
	if _, err := RegisterCatalog(srv.MCP(), CatalogOptions{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: &blockingDispatcher{entered: entered, release: release},
		PadVersion: "test",
	}); err != nil {
		t.Fatalf("RegisterCatalog: %v", err)
	}
	RegisterResources(srv.MCP(), &blockingFetcher{entered: entered, release: release}, nil)

	stdin, stdinW := io.Pipe()
	stdoutR, stdout := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- srv.RunStdio(context.Background(), stdin, stdout)
		_ = stdout.Close()
	}()

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

	// Give the read loop time to reach EOF, then let the calls finish.
	time.Sleep(300 * time.Millisecond)
	close(release)

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
