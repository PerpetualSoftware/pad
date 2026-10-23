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
	seconds := at.Add(time.Second).Unix()
	for _, tc := range []struct{ name, payload string }{
		{"non-JSON", `not JSON`},
		{"JSON null", `null`},
		{"JSON number", `123`},
		{"array", `[]`},
		{"no client_at", `{"busy":true}`},
		{"null client_at", `{"busy":true,"client_at":null}`},
		{"string client_at", `{"busy":true,"client_at":"invalid"}`},
		{"empty array client_at", `{"busy":true,"client_at":[]}`},
		{"missing seconds", `{"busy":true,"client_at":{"nanos":0}}`},
		{"missing nanos", fmt.Sprintf(`{"busy":true,"client_at":{"seconds":%d}}`, seconds)},
		{"string seconds", fmt.Sprintf(`{"busy":true,"client_at":{"seconds":%q,"nanos":0}}`, fmt.Sprint(seconds))},
		{"fractional seconds", fmt.Sprintf(`{"busy":true,"client_at":{"seconds":%d.5,"nanos":0}}`, seconds)},
		{"negative nanos", fmt.Sprintf(`{"busy":true,"client_at":{"seconds":%d,"nanos":-1}}`, seconds)},
		{"out-of-range nanos", fmt.Sprintf(`{"busy":true,"client_at":{"seconds":%d,"nanos":1000000000}}`, seconds)},
		{"huge seconds", `{"busy":true,"client_at":{"seconds":1e100,"nanos":0}}`},
		{"WRONGTYPE", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := p.activityKey(user.ID, presenceWorkerA)
			mr.Del(key)
			if tc.name == "WRONGTYPE" {
				mr.HSet(key, "busy", "true")
			} else if err := mr.Set(key, tc.payload); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"state":"idle","at":%q}`, at.Format(time.RFC3339Nano))
			rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, body)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("write over corrupt record: %d %s; want 204", rr.Code, rr.Body.String())
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
