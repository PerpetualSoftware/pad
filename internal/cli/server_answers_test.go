package cli

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TASK-3620. The health probe shares one per-address rate-limit bucket across
// every CLI on the machine, so a parallel fan-out drew 429s on it, and a probe
// that demanded a 200 read them as "no server": 206 `pad server start` spawns
// in two days, each opening the live database before losing the bind. A server
// that answers at all is holding the port. The spawn counter is the instrument,
// as in refresh_marker_test.go; TestEnsureServer_SpawnsWithoutAMarker is the
// control that the counter sees an ordinary auto-start.

// serveStatusAt answers every request on the config's port with status.
func serveStatusAt(t *testing.T, cfg *config.Config, status int) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		t.Fatalf("listen on %d: %v", cfg.Port, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(status)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func TestEnsureServer_DoesNotSpawnOverAServerThatAnswersNon200(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cfg, spawns := refreshTestConfig(t)
			serveStatusAt(t, cfg, status)

			if err := EnsureServer(cfg); err != nil {
				t.Fatalf("EnsureServer: %v", err)
			}
			if got := atomic.LoadInt32(spawns); got != 0 {
				t.Fatalf("spawns = %d, want 0: a server answering %d holds the port", got, status)
			}
			// The same answer must keep the database guards (pad db restore,
			// the migrations) from treating a throttled server as stopped.
			if !IsServerRunning(cfg) {
				t.Fatalf("IsServerRunning = false for a server answering %d", status)
			}
		})
	}
}

// The other half: `pad server start` binds before it migrates, so a starting
// server holds the port but answers nothing yet. A listener that never accepts
// is that window exactly; the kernel still completes the connect from its
// backlog. A probe that waited for an HTTP response timed out here and spawned
// a second start, which then lost the bind.
func TestEnsureServer_DoesNotSpawnOverAServerThatIsStillStarting(t *testing.T) {
	cfg, spawns := refreshTestConfig(t)
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		t.Fatalf("listen on %d: %v", cfg.Port, err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if err := EnsureServer(cfg); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if got := atomic.LoadInt32(spawns); got != 0 {
		t.Fatalf("spawns = %d, want 0: a bound port is held even before it serves", got)
	}
	if !IsServerRunning(cfg) {
		t.Fatal("IsServerRunning = false for a bound port that has not started serving")
	}
}
