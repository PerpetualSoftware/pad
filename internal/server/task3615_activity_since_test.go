package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3615 PR B: the activity feed's `since` accepts an RFC3339 instant as
// well as a date, so the catch-up card's "See everything" can open its exact
// window. Three rows are pinned 30 seconds apart. Every leg names the rows it
// expects, and a control leg one second later shows the filter actually bites.
func TestTASK3615_ActivitySinceAcceptsRFC3339(t *testing.T) {
	srv := testServer(t)
	f := newTask3615Fixture(t, srv)

	a := f.create(t, f.aliceTok, "tasks", "since A")
	b := f.create(t, f.aliceTok, "tasks", "since B")
	c := f.create(t, f.aliceTok, "tasks", "since C")
	for it, ts := range map[string]string{
		a.ID: "2026-10-01T10:00:00Z",
		b.ID: "2026-10-01T10:00:30Z",
		c.ID: "2026-10-01T10:01:00Z",
	} {
		if _, err := srv.store.DB().Exec(fmt.Sprintf("UPDATE activities SET created_at = '%s' WHERE document_id = '%s'", ts, it)); err != nil {
			t.Fatalf("pin activity: %v", err)
		}
	}
	name := map[string]string{a.ID: "A", b.ID: "B", c.ID: "C"}

	feed := func(t *testing.T, since string) (int, string) {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/activity?since="+url.QueryEscape(since), nil, f.aliceTok)
		if rr.Code != http.StatusOK {
			return rr.Code, rr.Body.String()
		}
		var acts []models.Activity
		parseJSON(t, rr, &acts)
		seen := map[string]bool{}
		for _, act := range acts {
			if n, ok := name[act.DocumentID]; ok {
				seen[n] = true
			}
		}
		var got []string
		for n := range seen {
			got = append(got, n)
		}
		sort.Strings(got)
		return rr.Code, strings.Join(got, ",")
	}

	for _, tc := range []struct{ leg, since, want string }{
		{"date form unchanged", "2026-10-01", "A,B,C"},
		{"Z instant", "2026-10-01T10:00:30Z", "B,C"},
		// The same instant as the Z leg. Read as a UTC wall time, 12:00:30
		// would be after every row and the answer would be empty.
		{"offset instant is converted, not read as UTC", "2026-10-01T12:00:30+02:00", "B,C"},
		// A fraction is dropped and the floor rounds DOWN, so B, stamped in
		// that same second, is re-shown and never hidden.
		{"fraction rounds the floor down", "2026-10-01T10:00:30.900Z", "B,C"},
		{"control: one second later drops B", "2026-10-01T10:00:31Z", "C"},
	} {
		t.Run(tc.leg, func(t *testing.T) {
			code, got := feed(t, tc.since)
			if code != http.StatusOK || got != tc.want {
				t.Fatalf("since=%s: code %d, got %q, want %q", tc.since, code, got, tc.want)
			}
		})
	}

	t.Run("garbage is refused 400 invalid_since", func(t *testing.T) {
		code, body := feed(t, "yesterday")
		if code != http.StatusBadRequest || !strings.Contains(body, "invalid_since") {
			t.Fatalf("code %d body %s", code, body)
		}
	})

	// An offset can carry the UTC instant into year 10000, whose cutoff text
	// "10000-…" sorts below every row, so the window would open to everything
	// (PR B r5). The control is the same wall time one hour earlier, which
	// stays in 9999 and is accepted.
	t.Run("a since whose UTC instant passes year 9999 is refused 400 invalid_since", func(t *testing.T) {
		code, body := feed(t, "9999-12-31T23:59:59-01:00")
		if code != http.StatusBadRequest || !strings.Contains(body, "invalid_since") {
			t.Fatalf("code %d body %s", code, body)
		}
		if code, got := feed(t, "9999-12-31T22:59:59-01:00"); code != http.StatusOK || got != "" {
			t.Fatalf("control: code %d, got %q, want 200 with no rows", code, got)
		}
	})
}
