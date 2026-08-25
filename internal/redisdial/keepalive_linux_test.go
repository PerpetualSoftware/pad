//go:build linux

package redisdial

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// TestKeepAliveIsActuallyAppliedToTheSocket is the assertion the constant test
// could not make, and the mutation matrix is what exposed the difference:
// deleting KeepAliveConfig from the dialer left the value-comparison test
// green, because comparing our copy against go-redis's published numbers says
// nothing about whether the dialer USES it.
//
// Linux-only by build tag. Reading SO_KEEPALIVE off the accepted socket is the
// only way to observe this from outside, and the syscall surface is not
// portable — the Smoke jobs on macOS and Windows skip the file entirely.
//
// WHAT THAT GAP ACTUALLY COSTS, since Pad ships darwin and windows binaries
// too (codex round 2, P3): nothing in regression terms. The behaviour is
// platform-independent — net.Dialer applies KeepAliveConfig on every platform
// Go supports — so a removal would be caught here, on the Go and Go (PostgreSQL)
// CI jobs, which are the ones that run the full suite. The macOS and Windows
// Smoke jobs are build-and-start checks and were never the guard for this.
// The gap is in OBSERVATION on those platforms, not in coverage of the change.
func TestKeepAliveIsActuallyAppliedToTheSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			time.AfterFunc(time.Second, func() { _ = c.Close() })
		}
	}()

	conn, err := New(nil, 5*time.Second)(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	var (
		keepalive int
		idle      int
		operr     error
	)
	if cerr := raw.Control(func(fd uintptr) {
		keepalive, operr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		if operr != nil {
			return
		}
		idle, operr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
	}); cerr != nil {
		t.Fatalf("control: %v", cerr)
	}
	if operr != nil {
		t.Fatalf("getsockopt: %v", operr)
	}

	if keepalive == 0 {
		t.Error("SO_KEEPALIVE is off: go-redis's default dialer enables it, and dropping it silently changes how " +
			"quickly a dead Redis peer is noticed on every connection this process holds")
	}
	if want := int(keepAliveConfig.Idle / time.Second); idle != want {
		t.Errorf("TCP_KEEPIDLE = %ds, want %ds — the configured idle period is not reaching the socket", idle, want)
	}
}
