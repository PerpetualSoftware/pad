package mcp

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestNewServer_Construction is the smoke test: NewServer with a
// zero-value Options must not panic, must return a usable Server, and
// must expose the underlying mcp-go server through MCP() so
// downstream tasks (TASK-945+) can register tools / resources before
// the transport starts.
func TestNewServer_Construction(t *testing.T) {
	t.Run("zero options", func(t *testing.T) {
		s := NewServer(Options{})
		if s == nil {
			t.Fatalf("NewServer returned nil")
		}
		if s.MCP() == nil {
			t.Errorf("MCP() returned nil — handlers cannot be registered")
		}
	})
	t.Run("explicit version + debug", func(t *testing.T) {
		s := NewServer(Options{Version: "1.2.3", Debug: true})
		if s == nil || s.MCP() == nil {
			t.Fatalf("NewServer should never return a partially-initialized server")
		}
	})
}

// TestServer_InitializeHandshake is the contract test: a real
// initialize round-trip through the stdio pipeline must return
// serverInfo with our canonical name + the version we passed in.
//
// This is what every MCP client (Claude Desktop, Cursor, Windsurf,
// mcp-inspector) sees on connect — if it regresses, downstream
// installations break silently.
func TestServer_InitializeHandshake(t *testing.T) {
	const wantVersion = "0.42.0-handshake-test"
	srv := NewServer(Options{Version: wantVersion})

	res, cleanup := runHandshake(t, srv)
	defer cleanup()

	if res.ServerInfo.Name != ServerName {
		t.Errorf("serverInfo.name = %q, want %q", res.ServerInfo.Name, ServerName)
	}
	if res.ServerInfo.Version != wantVersion {
		t.Errorf("serverInfo.version = %q, want %q", res.ServerInfo.Version, wantVersion)
	}
}

// TestServer_FallbackVersion locks the wire-level invariant that
// serverInfo.version is NEVER empty, even when the caller built the
// server with Options{}. Empty values would confuse some clients that
// display them in their UI.
func TestServer_FallbackVersion(t *testing.T) {
	srv := NewServer(Options{})

	res, cleanup := runHandshake(t, srv)
	defer cleanup()

	if res.ServerInfo.Version != FallbackVersion {
		t.Errorf("serverInfo.version = %q, want fallback %q",
			res.ServerInfo.Version, FallbackVersion)
	}
}

// TestServer_GracefulShutdownOnContextCancel verifies that cancelling
// the parent context (the path SIGINT / SIGTERM eventually take in
// production) returns RunStdio promptly with a nil error. Guards
// against goroutine leaks on shutdown.
func TestServer_GracefulShutdownOnContextCancel(t *testing.T) {
	srv := NewServer(Options{})

	stdin, stdinW := io.Pipe()
	stdoutR, stdout := io.Pipe()
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = stdoutR.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.RunStdio(ctx, stdin, stdout)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on ctx cancel, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("RunStdio did not return within 2s of cancel")
	}
}

// runHandshake spins up srv against in-memory pipes, drives the MCP
// client's Initialize, and returns the result + a cleanup that tears
// the goroutine down. Shared between the handshake-content tests so
// the wiring stays in one place.
func runHandshake(t *testing.T, srv *Server) (*mcp.InitializeResult, func()) {
	t.Helper()

	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.RunStdio(ctx, serverIn, serverOut)
	}()

	tport := transport.NewIO(clientIn, clientOut, io.NopCloser(strings.NewReader("")))
	if err := tport.Start(ctx); err != nil {
		cancel()
		_ = clientOut.Close()
		_ = serverOut.Close()
		wg.Wait()
		t.Fatalf("transport.Start: %v", err)
	}

	c := client.NewClient(tport)
	var req mcp.InitializeRequest
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{
		Name:    "pad-mcp-test-client",
		Version: "test",
	}
	res, err := c.Initialize(ctx, req)
	if err != nil {
		cancel()
		_ = tport.Close()
		_ = clientOut.Close()
		_ = serverOut.Close()
		wg.Wait()
		t.Fatalf("Initialize: %v", err)
	}

	cleanup := func() {
		_ = tport.Close()
		cancel()
		// Closing the writer halves unblocks the server's reader so
		// Listen returns and the goroutine exits.
		_ = clientOut.Close()
		_ = serverOut.Close()
		wg.Wait()
	}
	return res, cleanup
}
