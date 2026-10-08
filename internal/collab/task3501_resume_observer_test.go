package collab

import (
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TASK-3501: the Join path counts resumes (a cursor announced) and the
// force_refresh refusals of a resume, by reason. Each case asserts the exact
// counts, so a report fired twice, under the wrong reason, or for a since=0
// join all go red.

type recordingCollabObserver struct {
	mu        sync.Mutex
	resumes   int
	refreshes []string
}

func (o *recordingCollabObserver) ResumeJoined() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resumes++
}

func (o *recordingCollabObserver) ResumeForceRefreshed(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refreshes = append(o.refreshes, reason)
}

func (o *recordingCollabObserver) snapshot() (int, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resumes, append([]string(nil), o.refreshes...)
}

func dialWSQuery(t *testing.T, serverURL, itemID string, q url.Values) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(serverURL)
	c, _, err := websocket.DefaultDialer.Dial("ws://"+u.Host+"/ws/"+itemID+"?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func TestTASK3501_ResumeCounts(t *testing.T) {
	seed := func(t *testing.T, store *fakeOpLog, n int) {
		t.Helper()
		for i := 1; i <= n; i++ {
			if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, byte(i)}, "1"); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	for _, tc := range []struct {
		name        string
		setup       func(t *testing.T, store *fakeOpLog, mgr *RoomManager)
		since       int64
		contentSeq  int64
		wantType    string
		wantResumes int
		wantRefresh []string
	}{
		{"fresh join (since=0) is not a resume", func(t *testing.T, s *fakeOpLog, _ *RoomManager) { seed(t, s, 2) },
			0, 0, ControlMessageOpLogCursor, 0, nil},
		{"admitted resume", func(t *testing.T, s *fakeOpLog, _ *RoomManager) { seed(t, s, 4) },
			2, 0, "", 1, nil},
		{"resume against an empty op-log is pruned", func(*testing.T, *fakeOpLog, *RoomManager) {},
			5, 0, ControlMessageForceRefresh, 1, []string{ResumeRefreshPruned}},
		{"resume below MIN is behind_min", func(t *testing.T, s *fakeOpLog, _ *RoomManager) {
			seed(t, s, 2)
			s.mu.Lock()
			s.rows = s.rows[1:]
			s.mu.Unlock()
		}, 1, 0, ControlMessageForceRefresh, 1, []string{ResumeRefreshBehindMin}},
		{"resume seeded before a restore is restored", func(t *testing.T, s *fakeOpLog, m *RoomManager) {
			seed(t, s, 2)
			m.SetLastRestoreSeq("item-a", 10)
		}, 2, 5, ControlMessageForceRefresh, 1, []string{ResumeRefreshRestored}},
		{"fresh join seeded before a restore is refreshed but not counted", func(t *testing.T, s *fakeOpLog, m *RoomManager) {
			seed(t, s, 2)
			m.SetLastRestoreSeq("item-a", 10)
		}, 0, 5, ControlMessageForceRefresh, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := NewMemoryOpBus()
			defer bus.Close()
			store := &fakeOpLog{}
			mgr := NewRoomManager(store, bus)
			defer mgr.Close()
			obs := &recordingCollabObserver{}
			mgr.SetObserver(obs)
			tc.setup(t, store, mgr)

			srv := newCollabTestServer(t, mgr)
			defer srv.Close()
			q := url.Values{}
			if tc.since > 0 {
				q.Set("since", strconv.FormatInt(tc.since, 10))
			}
			if tc.contentSeq > 0 {
				q.Set("content_seq", strconv.FormatInt(tc.contentSeq, 10))
			}
			c := dialWSQuery(t, srv.URL, "item-a", q)
			defer c.Close()

			if tc.wantType != "" {
				if ctl := readControlWithin(t, c, time.Second); ctl.Type != tc.wantType {
					t.Fatalf("first control frame %q, want %q", ctl.Type, tc.wantType)
				}
			} else {
				// Admitted: the replayed rows, then the cursor.
				readBinaryWithin(t, c, time.Second)
				readBinaryWithin(t, c, time.Second)
				if ctl := readControlWithin(t, c, time.Second); ctl.Type != ControlMessageOpLogCursor {
					t.Fatalf("admitted resume: first control frame %q, want the cursor", ctl.Type)
				}
			}

			resumes, refreshes := obs.snapshot()
			if resumes != tc.wantResumes {
				t.Errorf("resumes = %d, want %d", resumes, tc.wantResumes)
			}
			if len(refreshes) != len(tc.wantRefresh) || (len(refreshes) == 1 && refreshes[0] != tc.wantRefresh[0]) {
				t.Errorf("refreshes = %v, want %v", refreshes, tc.wantRefresh)
			}
		})
	}
}
