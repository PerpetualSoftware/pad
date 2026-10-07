package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// revalWarnCounter counts the collab revalidation-error warnings, passing
// every record on to the handler it wraps.
type revalWarnCounter struct {
	slog.Handler
	mu    *sync.Mutex
	count *int
}

func (h revalWarnCounter) Handle(ctx context.Context, rec slog.Record) error {
	// The stream helper's message too: before BUG-3474 the collab tick's
	// credential re-check logged through it, so the sessions leg reaches its
	// real assertion (does the frame persist?) on the unfixed code.
	if strings.HasPrefix(rec.Message, "collab: revalidation") || strings.HasPrefix(rec.Message, "stream: credential re-check failed") {
		h.mu.Lock()
		*h.count++
		h.mu.Unlock()
	}
	return h.Handler.Handle(ctx, rec)
}

func (h revalWarnCounter) WithAttrs(a []slog.Attr) slog.Handler {
	return revalWarnCounter{Handler: h.Handler.WithAttrs(a), mu: h.mu, count: h.count}
}

func (h revalWarnCounter) WithGroup(n string) slog.Handler {
	return revalWarnCounter{Handler: h.Handler.WithGroup(n), mu: h.mu, count: h.count}
}

// BUG-3474 (lead ruling): a revalidation tick that ERRORS must not leave the
// connection with the write permission it last had. It used to keep both the
// connection and its write flag, so an editor demoted while the store was
// failing kept writing until the first clean tick. Now the connection is kept
// but goes read-only, and the next clean tick restores write if the role
// still allows it.
//
// The faults are real, one per error branch of the tick: renaming sessions
// makes the credential re-check UNKNOWN (codex r1: it used to read as valid,
// so that tick went on and set write from the role); renaming
// workspace_members fails the membership lookup inside authorizeCollabAccess
// with a plain (non-denial) error; renaming items fails the tick's own
// GetItem. Frames persist to item_yjs_updates, which no rename touches.
func TestBUG3474_RevalErrorFailsClosedOnWrite(t *testing.T) {
	for _, table := range []string{"sessions", "workspace_members", "items"} {
		t.Run(table, func(t *testing.T) { runBUG3474FaultLeg(t, table) })
	}
}

func runBUG3474FaultLeg(t *testing.T, table string) {
	origInterval := collabMembershipRevalInterval
	collabMembershipRevalInterval = 25 * time.Millisecond
	defer func() { collabMembershipRevalInterval = origInterval }()

	var mu sync.Mutex
	warns := 0
	// A fresh handler, not slog.Default().Handler(): the initial default
	// handler writes through the log package, which routes back into slog
	// once SetDefault replaces it, and the wrapped call deadlocks on log's
	// own mutex.
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(revalWarnCounter{Handler: slog.NewTextHandler(io.Discard, nil), mu: &mu, count: &warns}))
	defer slog.SetDefault(prevLogger)
	warnCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return warns
	}

	srv := testServerWithCollab(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Reval fault"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: "fault@test.com", Name: "Fault", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	token, err := srv.store.CreateSession(u.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookies := []*http.Cookie{{Name: "pad_session", Value: token}}

	conn, resp, err := dialCollab(t, ts.URL, item.ID, cookies, "go-test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	// The control: an editor's frame persists.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0xE1}); err != nil {
		t.Fatalf("editor write: %v", err)
	}
	waitForOpLogRows(t, srv, item.ID, 1, 3*time.Second)

	// The fault, and its premise: a tick has met it. The warning is logged
	// AFTER the connection is made read-only, so from here a frame must drop.
	before := warnCount()
	if _, err := srv.store.DB().Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_bug3474`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if _, err := srv.store.DB().Exec(`ALTER TABLE ` + table + `_bug3474 RENAME TO ` + table); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	defer restore()
	deadline := time.Now().Add(3 * time.Second)
	for warnCount() == before {
		if time.Now().After(deadline) {
			t.Fatal("no revalidation tick met the fault within 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	faulted := []byte{0x00, 0x22}
	if err := conn.WriteMessage(websocket.BinaryMessage, faulted); err != nil {
		t.Fatalf("faulted write: %v", err)
	}
	// In-process persistence is sub-millisecond, so a leaked frame usually
	// shows inside this window. The decisive check is the one after recovery.
	settle := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(settle) {
		rows, err := srv.store.LoadYjsUpdatesSince(item.ID, 0)
		if err != nil {
			t.Fatalf("LoadYjsUpdatesSince: %v", err)
		}
		if len(rows) > 1 {
			t.Fatalf("a frame sent while revalidation was failing persisted (%d rows): the connection kept write", len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Write comes back after a clean tick: still an editor, so the next
	// successful revalidation restores it. Frames are re-sent until one of
	// THEM lands (each distinct, so none is deduplicated), because the clean
	// tick's timing is not observable from here. One connection's frames are
	// handled in order, so once a recovery frame has landed the faulted frame
	// has been handled too, and its absence is then a fact rather than a
	// window (codex r1: a count could not tell the two apart).
	restore()
	sent := map[string]bool{}
	landed := false
	resend := time.Now().Add(5 * time.Second)
	var rows []models.YjsUpdate
	for i := byte(0); time.Now().Before(resend) && !landed; i++ {
		frame := []byte{0x00, 0x30, i}
		sent[string(frame)] = true
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("recovered write: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		rows, err = srv.store.LoadYjsUpdatesSince(item.ID, 0)
		if err != nil {
			t.Fatalf("LoadYjsUpdatesSince: %v", err)
		}
		for _, row := range rows {
			if sent[string(row.UpdateData)] {
				landed = true
			}
		}
	}
	if !landed {
		t.Fatal("write never came back after the store recovered")
	}
	for _, row := range rows {
		if bytes.Equal(row.UpdateData, faulted) {
			t.Fatal("the frame sent while revalidation was failing persisted: the connection kept write")
		}
	}
}
