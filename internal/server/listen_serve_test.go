package server

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// BUG-2965 codex round 2: `pad server start` must be able to tell "we own this
// port" from "we are about to try", because the PID file it writes names the
// process `pad server stop` will signal. Splitting Listen from Serve is what
// makes that distinguishable; these pin the split itself.

func TestListen_RefusesAPortAlreadyHeld(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	defer held.Close()

	srv := testServer(t)
	ln, err := srv.Listen(held.Addr().String())
	if err == nil {
		ln.Close()
		t.Fatal("Listen succeeded on a held port — a losing start would then claim the PID file")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Logf("bind error is %v (not EADDRINUSE); the refusal is what matters", err)
	}
}

func TestListenAndServe_StillServes(t *testing.T) {
	srv := testServer(t)
	ln, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })

	// The split must not have changed what a bound server does: it answers.
	// A nil Transport means http.DefaultTransport, which another test's
	// httptest server teardown reaches into (BUG-3008). The timeout is the
	// reason this is not a bare isolatedTestClient() call.
	client := isolatedTestClient()
	client.Timeout = 2 * time.Second
	var resp *http.Response
	for i := 0; i < 20; i++ {
		resp, err = client.Get("http://" + ln.Addr().String() + "/api/v1/health")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("health probe never succeeded: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
}
