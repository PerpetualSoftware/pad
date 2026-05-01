package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
)

// Server is pad's MCP server. It wraps a mark3labs/mcp-go MCPServer
// plus the stdio transport.
//
// TASK-944 lays the bones — handshake-only, no tools / resources /
// prompts yet. Subsequent tasks layer the cmdhelp-derived tool
// registry (TASK-945), resources (TASK-946), and prompts (TASK-947)
// onto MCP() before Run() takes over the transport.
type Server struct {
	mcp   *server.MCPServer
	debug bool
}

// Options configures NewServer. All fields are optional.
type Options struct {
	// Version is reported to the client in the initialize handshake's
	// serverInfo.version. If empty, FallbackVersion is used.
	Version string

	// Debug enables verbose stderr logging from the MCP server.
	// stdout is reserved for JSON-RPC traffic; logs always go to stderr.
	Debug bool
}

// NewServer constructs a pad MCP server. Call Run(ctx) to start it.
//
// The returned Server is wired with tool capabilities advertised but
// no tools registered — that's TASK-945's job. Callers that need to
// register tools / resources / prompts before Run() can do so via
// the MCP() accessor.
func NewServer(opts Options) *Server {
	version := opts.Version
	if version == "" {
		version = FallbackVersion
	}
	mcp := server.NewMCPServer(
		ServerName,
		version,
		// Declare tool capability up-front so clients know the server
		// CAN serve tools. The list stays empty until TASK-945 wires
		// in the cmdhelp-derived registry.
		server.WithToolCapabilities(true),
		// Recover from panics in tool handlers — important once the
		// shell-out dispatcher (TASK-945) starts running pad
		// subprocesses with arbitrary client-supplied args.
		server.WithRecovery(),
		// Tool-surface stability tier (TASK-963). Surfaces the cmdhelp
		// contract directly in the handshake at
		// capabilities.experimental.padCmdhelp, so external agents can
		// detect compatibility without reading pad://_meta/version first.
		server.WithExperimental(experimentalCapabilities()),
		// Server-level instructions (TASK-971). Tells agents WHEN to
		// reach for pad and orients them to the tool surface +
		// resources before they make their first call. Embedded at
		// build time from instructions.md so the source of truth is
		// versioned with the binary; HTTPHandlerDispatcher (PLAN-943)
		// advertises the same string.
		server.WithInstructions(Instructions),
	)
	return &Server{mcp: mcp, debug: opts.Debug}
}

// MCP returns the underlying mark3labs MCPServer. Use this to register
// tools / resources / prompts BEFORE Run() takes over the stdio
// transport — registrations made after Run() are racy.
func (s *Server) MCP() *server.MCPServer {
	return s.mcp
}

// Run starts the stdio transport on os.Stdin / os.Stdout. It blocks
// until ctx is cancelled, stdin reaches EOF, or SIGINT / SIGTERM is
// received. EOF and signals are clean-shutdown paths and return nil.
func (s *Server) Run(ctx context.Context) error {
	return s.RunStdio(ctx, os.Stdin, os.Stdout)
}

// RunStdio is Run with explicit stdin / stdout streams. Used by tests
// that drive the server in-process via io.Pipe.
//
// The same shutdown semantics as Run apply: EOF, ctx cancel, and
// closed-pipe are clean (return nil); anything else is wrapped and
// returned.
func (s *Server) RunStdio(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	stdio := server.NewStdioServer(s.mcp)
	stdio.SetErrorLogger(s.errLogger())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// SIGINT / SIGTERM trigger graceful shutdown by cancelling ctx,
	// which propagates into stdio.Listen's select loop. Stop the
	// notifier on return so we don't leak goroutines / signal handlers
	// when the test harness re-runs.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		select {
		case <-sigChan:
			cancel()
		case <-ctx.Done():
		}
	}()

	err := stdio.Listen(ctx, stdin, stdout)
	if err == nil {
		return nil
	}
	// EOF / ctx-cancelled / closed-pipe are clean shutdown paths.
	// io.ErrClosedPipe shows up when test harnesses close the input
	// half mid-flight — semantically the same as EOF for our purposes.
	if errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return fmt.Errorf("mcp stdio: %w", err)
}

func (s *Server) errLogger() *log.Logger {
	prefix := ""
	if s.debug {
		prefix = "pad-mcp: "
	}
	return log.New(os.Stderr, prefix, log.LstdFlags)
}
