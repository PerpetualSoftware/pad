package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3501, CONVE-19: wiring is a claim. The collab package proves the Join
// path reports and the metrics package proves the adapter maps it, and both
// pass with the production SetObserver deleted. This drives the production
// constructor against a real store: a resume against an empty op-log must
// move both counters.
func TestTheCollabRoomManagerReportsToMetrics(t *testing.T) {
	s := storetest.NewSQLite(t)
	m := metrics.New()
	bus := collab.NewMemoryOpBus()
	t.Cleanup(bus.Close)
	rm := newObservedRoomManager(s, bus, m)
	t.Cleanup(rm.Close)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = rm.Join("item-3501", conn, 5, 0, true, false, nil)
	}))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, msg, err := c.ReadMessage(); err != nil || !strings.Contains(string(msg), collab.ControlMessageForceRefresh) {
		t.Fatalf("want a force_refresh frame, got %q (err %v)", msg, err)
	}

	if got := gatheredCounter(t, m, "pad_collab_resumes_total", ""); got != 1 {
		t.Errorf("pad_collab_resumes_total = %v, want 1", got)
	}
	if got := gatheredCounter(t, m, "pad_collab_resume_force_refreshes_total", collab.ResumeRefreshPruned); got != 1 {
		t.Errorf("pad_collab_resume_force_refreshes_total{reason=pruned} = %v, want 1", got)
	}
}

// gatheredCounter reads a counter from the registry; reason "" means the
// unlabelled series.
func gatheredCounter(t *testing.T, m *metrics.Metrics, name, reason string) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			match := reason == ""
			for _, l := range metric.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					match = true
				}
			}
			if match {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// RunE must build the manager through the observed constructor; a bare
// collab.NewRoomManager there would compile and leave every counter at zero.
func TestServerStartUsesTheObservedRoomManager(t *testing.T) {
	src, err := os.ReadFile("cmd_server.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "srv.SetCollabRoomManager(newObservedRoomManager(") {
		t.Error("serverStartCmd no longer builds the room manager through newObservedRoomManager")
	}
	if n := strings.Count(body, "collab.NewRoomManager("); n != 1 {
		t.Errorf("collab.NewRoomManager( appears %d times in cmd_server.go, want exactly 1 (inside newObservedRoomManager)", n)
	}
}
