package server

import (
	"bytes"
	"context"
	"mime/multipart"
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

// testCSRFToken is the double-submit pair doRequestWithCookie sends.
const testCSRFToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func authedReq(method, path string, body *bytes.Buffer, contentType, cookie string, headers map[string]string) *http.Request {
	if body == nil {
		body = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: cookie})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRFToken})
	req.Header.Set("X-CSRF-Token", testCSRFToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// funnelOwner bootstraps an owner with a workspace and a seeded item and
// parent comment, written through the store so the account has not written.
func funnelOwner(t *testing.T, srv *Server) (cookie string, ws models.Workspace, item *models.Item, parent *models.Comment) {
	t.Helper()
	cookie = bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Funnel"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &ws)
	coll, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("collection: %v", err)
	}
	item, err = srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Seeded", CreatedBy: "user"})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	owner, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	parent, err = srv.store.CreateComment(ws.ID, item.ID, owner.ID, models.CommentCreate{Body: "parent", CreatedBy: "user"})
	if err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	return cookie, ws, item, parent
}

// Every call site, through its handler, as the account's first write: the
// reply route, the attachment upload, and an item create by an agent.
func TestFirstWriteMetricEachCallSite(t *testing.T) {
	t.Run("reply", func(t *testing.T) {
		srv := testServer(t)
		m := metrics.New()
		srv.SetMetrics(m)
		cookie, ws, _, parent := funnelOwner(t, srv)
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/comments/"+parent.ID+"/replies", map[string]any{"body": "reply"}, cookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("reply: %d %s", rr.Code, rr.Body.String())
		}
		if got, total := firstWrites(t, m, "comment", "web"), totalFirstWrites(t, m); got != 1 || total != 1 {
			t.Fatalf("reply as first write: comment/web = %v, total = %v, want 1 and 1", got, total)
		}
	})
	t.Run("attachment", func(t *testing.T) {
		srv, _ := testServerWithAttachments(t)
		m := metrics.New()
		srv.SetMetrics(m)
		cookie, ws, _, _ := funnelOwner(t, srv)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", "pixel.png")
		part.Write(realPNG())
		mw.Close()
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, authedReq("POST", "/api/v1/workspaces/"+ws.Slug+"/attachments", &buf, mw.FormDataContentType(), cookie, nil))
		if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
			t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
		}
		if got, total := firstWrites(t, m, "attachment", "web"), totalFirstWrites(t, m); got != 1 || total != 1 {
			t.Fatalf("upload as first write: attachment/web = %v, total = %v, want 1 and 1", got, total)
		}
	})
	t.Run("item by an agent", func(t *testing.T) {
		srv := testServer(t)
		m := metrics.New()
		srv.SetMetrics(m)
		cookie, ws, _, _ := funnelOwner(t, srv)
		body := bytes.NewBufferString(`{"title":"From the agent"}`)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, authedReq("POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", body, "application/json", cookie, map[string]string{"X-Pad-Agent": "claude-code"}))
		if rr.Code != http.StatusCreated {
			t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
		}
		if got, total := firstWrites(t, m, "item", "agent"), totalFirstWrites(t, m); got != 1 || total != 1 {
			t.Fatalf("agent item as first write: item/agent = %v, total = %v, want 1 and 1", got, total)
		}
	})
}

// A workspace restore records the write but is not an item write, so as an
// account's first write it is not counted (TASK-3926 r1).
func TestFirstWriteMetricSkipsWorkspaceRestore(t *testing.T) {
	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	cookie, ws, _, _ := funnelOwner(t, srv)
	rr := doRequestWithCookie(srv, "DELETE", "/api/v1/workspaces/"+ws.Slug, nil, cookie)
	if rr.Code >= 300 {
		t.Fatalf("archive workspace: %d %s", rr.Code, rr.Body.String())
	}
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	// Deleting a workspace does not go through the write helper, so the
	// restore below is the account's first write.
	if owner.LastWriteAt != "" {
		t.Fatalf("precondition: the account already wrote (last_write_at=%q)", owner.LastWriteAt)
	}
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/restore", nil, cookie)
	if rr.Code >= 300 {
		t.Fatalf("restore workspace: %d %s", rr.Code, rr.Body.String())
	}
	if total := totalFirstWrites(t, m); total != 0 {
		t.Fatalf("a workspace restore counted %v first writes, want 0", total)
	}
	after, _ := srv.store.GetUserByEmail("owner@test.com")
	if after.LastWriteAt == "" {
		t.Fatal("the restore no longer records the write (last_write_at still empty)")
	}
}

// The bootstrap admin is not a signup.
func TestSignupMetricSkipsBootstrap(t *testing.T) {
	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	if web, app := signups(t, m, "web"), signups(t, m, "app"); web != 0 || app != 0 {
		t.Fatalf("bootstrap counted as a signup: web = %v app = %v", web, app)
	}
}
