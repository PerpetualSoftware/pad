package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3926: the app funnel's server half. pad_signups_total by client,
// pad_first_writes_total / pad_first_write_age_seconds for a new human
// account's first write.

const padShellUA = "Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 PadShell/1"

func signups(t *testing.T, m *metrics.Metrics, client string) float64 {
	t.Helper()
	return counterValue(t, m.SignupsTotal.WithLabelValues("email", client))
}

func firstWrites(t *testing.T, m *metrics.Metrics, kind, client string) float64 {
	t.Helper()
	return counterValue(t, m.FirstWritesTotal.WithLabelValues(kind, client))
}

// totalFirstWrites sums every pre-created series, so a count under the wrong
// label still shows up as a count.
func totalFirstWrites(t *testing.T, m *metrics.Metrics) float64 {
	t.Helper()
	var n float64
	for _, kind := range metrics.FirstWriteKinds {
		for _, client := range metrics.WriteClients {
			n += firstWrites(t, m, kind, client)
		}
	}
	return n
}

// A self-serve signup is counted by client: the PadShell user agent is the
// app, anything else the web. An admin creating someone's account is not a
// signup.
func TestSignupMetricClientLabel(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	m := metrics.New()
	srv.SetMetrics(m)

	register := func(email, ua string) {
		t.Helper()
		headers := map[string]string{}
		if ua != "" {
			headers["User-Agent"] = ua
		}
		rr := doRequestWithHeaders(srv, "POST", "/api/v1/auth/register", map[string]string{
			"email": email, "name": "New", "password": "correct-horse-battery-staple",
		}, headers)
		if rr.Code != http.StatusCreated {
			t.Fatalf("register %s: %d %s", email, rr.Code, rr.Body.String())
		}
	}

	register("app@example.com", padShellUA)
	if got := signups(t, m, "app"); got != 1 {
		t.Fatalf("PadShell signup: app = %v, want 1", got)
	}
	register("web@example.com", "")
	if got, app := signups(t, m, "web"), signups(t, m, "app"); got != 1 || app != 1 {
		t.Fatalf("signup with no user agent: web = %v app = %v, want 1 and 1", got, app)
	}

	// Admin-created: the admin is signed in and creates another account.
	admin := loginUser(t, srv, "admin@pad.test", "correct-horse-battery-staple")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "made@example.com", "name": "Made", "password": "correct-horse-battery-staple",
	}, admin)
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin-created register: %d %s", rr.Code, rr.Body.String())
	}
	if web, app := signups(t, m, "web"), signups(t, m, "app"); web != 1 || app != 1 {
		t.Fatalf("an admin-created account was counted: web = %v app = %v, want 1 and 1", web, app)
	}
}

// Through the real handlers: a new account's first item is counted once,
// under kind=item and client=web; a second item and a comment after it are
// not first writes.
func TestFirstWriteMetricOnceAndWindow(t *testing.T) {
	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	cookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Funnel"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	if got := totalFirstWrites(t, m); got != 0 {
		t.Fatalf("creating a workspace counted %v first writes, want 0", got)
	}

	var item models.Item
	for i, title := range []string{"One", "Two"} {
		rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]any{"title": title}, cookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create item %d: %d %s", i, rr.Code, rr.Body.String())
		}
		if i == 0 {
			parseJSON(t, rr, &item)
		}
	}
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug+"/comments", map[string]any{"body": "hi"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	if got := firstWrites(t, m, "item", "web"); got != 1 {
		t.Fatalf("first_writes{item,web} = %v, want 1", got)
	}
	if got := totalFirstWrites(t, m); got != 1 {
		t.Fatalf("first writes total = %v, want 1", got)
	}
	if n := histogramCount(t, m, "web"); n != 1 {
		t.Fatalf("first_write_age_seconds{web} count = %d, want 1", n)
	}
}

// A comment that is the account's first write is counted as kind=comment
// (the attachment kind is covered by TestRecordFirstWriteGates' control).
func TestFirstWriteMetricKindFromCommentSite(t *testing.T) {
	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	cookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Funnel"}, cookie)
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	// Seed the item in the store directly, so the account has not written yet.
	coll, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("collection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Seeded", CreatedBy: "user"})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug+"/comments", map[string]any{"body": "first"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	if got := firstWrites(t, m, "comment", "web"); got != 1 {
		t.Fatalf("first_writes{comment,web} = %v, want 1 (total %v)", got, totalFirstWrites(t, m))
	}
}

func histogramCount(t *testing.T, m *metrics.Metrics, client string) uint64 {
	t.Helper()
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "pad_first_write_age_seconds" {
			continue
		}
		for _, mm := range f.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() == "client" && lp.GetValue() == client {
					return mm.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// recordFirstWrite's gates, directly: the 30-day window, an app principal,
// and a call that was not a first write.
func TestRecordFirstWriteGates(t *testing.T) {
	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	req := func(u *models.User) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		return r.WithContext(context.WithValue(r.Context(), ctxCurrentUser, u))
	}
	fresh := &models.User{ID: "u1", Kind: models.UserKindHuman, CreatedAt: time.Now().Add(-time.Hour)}

	srv.recordFirstWrite(req(fresh), writeKindItem, false)
	if got := totalFirstWrites(t, m); got != 0 {
		t.Fatalf("a non-first write counted: %v", got)
	}
	srv.recordFirstWrite(req(&models.User{ID: "u2", Kind: models.UserKindHuman, CreatedAt: time.Now().Add(-60 * 24 * time.Hour)}), writeKindItem, true)
	if got := totalFirstWrites(t, m); got != 0 {
		t.Fatalf("a 60-day-old account's first write counted: %v", got)
	}
	srv.recordFirstWrite(req(&models.User{ID: "u3", Kind: models.UserKindApp, CreatedAt: time.Now()}), writeKindItem, true)
	if got := totalFirstWrites(t, m); got != 0 {
		t.Fatalf("an app principal's first write counted: %v", got)
	}
	srv.recordFirstWrite(req(nil), writeKindItem, true)
	if got := totalFirstWrites(t, m); got != 0 {
		t.Fatalf("a request with no user counted: %v", got)
	}
	// Control: inside the window it counts, under the right labels.
	srv.recordFirstWrite(req(fresh), writeKindAttachment, true)
	if got := firstWrites(t, m, "attachment", "web"); got != 1 {
		t.Fatalf("control: first_writes{attachment,web} = %v, want 1", got)
	}
	srv.recordFirstWrite(req(&models.User{ID: "u4", Kind: models.UserKindHuman, CreatedAt: time.Now().Add(-29 * 24 * time.Hour)}), writeKindItem, true)
	if got := firstWrites(t, m, "item", "web"); got != 1 {
		t.Fatalf("control: a 29-day-old account's first write = %v, want 1", got)
	}
}

func TestWriteClientLabel(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"browser", nil, "web"},
		{"shell", map[string]string{"User-Agent": padShellUA}, "app"},
		{"bearer", map[string]string{"Authorization": "Bearer pad_x"}, "cli"},
		{"agent over the cli", map[string]string{"Authorization": "Bearer pad_x", "X-Pad-Agent": "claude-code"}, "agent"},
		{"agent in the shell", map[string]string{"User-Agent": padShellUA, "X-Pad-Agent": "x"}, "agent"},
		{"shell with a bearer", map[string]string{"User-Agent": padShellUA, "Authorization": "Bearer pad_x"}, "app"},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := writeClient(r); got != c.want {
			t.Errorf("%s: writeClient = %q, want %q", c.name, got, c.want)
		}
	}
	if got := signupClient(httptest.NewRequest("POST", "/", nil)); got != "web" {
		t.Errorf("signupClient(no UA) = %q", got)
	}
}
