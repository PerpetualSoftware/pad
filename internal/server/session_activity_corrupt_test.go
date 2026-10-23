package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestSessionActivityRedisReplacesCorruptClientTimestamp(t *testing.T) {
	srv := testServerWithPresence(t)
	p, _, mr := newRedisPresencePair(t)
	srv.SetSessionPresence(p)
	_, _, tok, user := setupWatchTestUser(t, srv)
	at := time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC)
	for _, tc := range []struct{ name, clientAt string }{
		{"missing nanos", fmt.Sprintf(`{"seconds":%d}`, at.Unix())},
		{"string seconds", fmt.Sprintf(`{"seconds":%q,"nanos":0}`, fmt.Sprint(at.Unix()))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := p.activityKey(user.ID, presenceWorkerA)
			if err := mr.Set(key, `{"busy":true,"client_at":`+tc.clientAt+`}`); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"state":"idle","at":%q}`, at.Format(time.RFC3339Nano))
			rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, body)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("write over corrupt client_at: %d %s; want 204", rr.Code, rr.Body.String())
			}
			payload, err := mr.Get(key)
			if err != nil {
				t.Fatal(err)
			}
			var got sessionActivity
			if err := json.Unmarshal([]byte(payload), &got); err != nil {
				t.Fatal(err)
			}
			wantAt := activityTimestamp(&at)
			if got.Busy || got.At.IsZero() || got.ClientAt == nil || *got.ClientAt != *wantAt {
				t.Fatalf("corrupt record was not replaced: %s", payload)
			}
		})
	}
}
