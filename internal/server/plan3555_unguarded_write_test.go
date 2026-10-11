package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/metrics"
	dto "github.com/prometheus/client_model/go"
)

// PLAN-3555 phase 1: every unguarded item update is counted by door and kind,
// a guarded or deliberate one is not, an append-only one is not, and only an
// agent door's unguarded CONTENT write is told so in the response.
func TestPLAN3555_UnguardedWritesCountedByDoorAndKind(t *testing.T) {
	srv := testServer(t)
	m := metrics.New()
	srv.SetMetrics(m)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	cli := map[string]string{"X-Pad-Client": "cli"}

	count := func(door, kind string) float64 {
		t.Helper()
		var out dto.Metric
		if err := m.ContentWritesUnguardedTotal.WithLabelValues(door, kind).Write(&out); err != nil {
			t.Fatal(err)
		}
		return out.GetCounter().GetValue()
	}
	patch := func(headers map[string]string, body map[string]any) bool {
		t.Helper()
		rr := doRequestWithHeaders(srv, "PATCH", path, body, headers)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH %v: %d %s", body, rr.Code, rr.Body.String())
		}
		var got struct {
			Seq      int64          `json:"seq"`
			Warnings map[string]any `json:"warnings"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		flagged, _ := got.Warnings["unguarded_content_write"].(bool)
		return flagged
	}
	seq := func() int64 {
		t.Helper()
		it, err := srv.store.GetItem(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		return it.Seq
	}
	type want struct{ cliContent, cliFields, webContent float64 }
	check := func(step string, w want) {
		t.Helper()
		if a, b, c := count("cli", "content"), count("cli", "fields"), count("web", "content"); a != w.cliContent || b != w.cliFields || c != w.webContent {
			t.Fatalf("%s: cli/content=%v cli/fields=%v web/content=%v, want %v/%v/%v", step, a, b, c, w.cliContent, w.cliFields, w.webContent)
		}
	}

	if !patch(cli, map[string]any{"content": "one"}) {
		t.Fatal("an unguarded CLI content write must carry warnings.unguarded_content_write")
	}
	check("unguarded CLI content", want{1, 0, 0})

	if patch(cli, map[string]any{"content": "two", "expected_seq": seq()}) {
		t.Fatal("a token-guarded content write was flagged unguarded")
	}
	check("seq-guarded CLI content", want{1, 0, 0})

	if patch(cli, map[string]any{"content": "three", "replace": true}) {
		t.Fatal("a replace content write was flagged unguarded")
	}
	check("replace CLI content", want{1, 0, 0})

	patch(cli, map[string]any{"append_implementation_note": map[string]any{"summary": "a note"}})
	check("append-only CLI write", want{1, 0, 0})

	if patch(nil, map[string]any{"content": "four"}) {
		t.Fatal("a browser write was flagged: the warning is for agent doors only")
	}
	check("unguarded web content", want{1, 0, 1})

	// Last: a rename moves the slug the path above is built from.
	if patch(cli, map[string]any{"title": "Renamed"}) {
		t.Fatal("a field-only write was flagged as an unguarded CONTENT write")
	}
	check("unguarded CLI title", want{1, 1, 1})
}

// PLAN-3555: a seq conflict on a BODY write says not to resend the body and
// carries content_write, changed_at and changed_by_kind; the same conflict on
// a field-only write keeps the old answer.
func TestPLAN3555_ContentConflictSaysDoNotResend(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	read, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else's write lands after our read, from an agent.
	if rr := doRequestWithHeaders(srv, "PATCH", path, map[string]any{"content": "theirs"}, map[string]string{"X-Pad-Agent": "other"}); rr.Code != http.StatusOK {
		t.Fatalf("premise write: %d %s", rr.Code, rr.Body.String())
	}
	now, _ := srv.store.GetItem(item.ID)

	refuse := func(body map[string]any) (string, map[string]any) {
		t.Helper()
		rr := doRequest(srv, "PATCH", path, body)
		if rr.Code != http.StatusConflict {
			t.Fatalf("stale seq must conflict; got %d %s", rr.Code, rr.Body.String())
		}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		if env.Error.Code != "update_conflict" {
			t.Fatalf("code = %q", env.Error.Code)
		}
		return env.Error.Message, env.Error.Details
	}

	msg, d := refuse(map[string]any{"content": "mine", "expected_seq": read.Seq})
	if d["content_write"] != true || d["changed_by_kind"] != "agent" || d["changed_at"] == nil {
		t.Fatalf("content conflict details = %v", d)
	}
	if !strings.Contains(msg, "Do not resend your body") || !strings.Contains(msg, "→") {
		t.Fatalf("content conflict message = %q", msg)
	}
	if got := int64(d["actual_seq"].(float64)); got != now.Seq {
		t.Fatalf("actual_seq = %d, want %d", got, now.Seq)
	}

	msg, d = refuse(map[string]any{"title": "Mine", "expected_seq": read.Seq})
	if _, ok := d["content_write"]; ok || strings.Contains(msg, "resend") {
		t.Fatalf("a field-only conflict took the content answer: %q %v", msg, d)
	}
}

// PLAN-3555: on the APPLIER path the row write carries no content (it travels
// to the tab), so the store cannot know the request was a body write; the
// route says so. Without that, a stale content write over an open tab would
// get the field-conflict answer.
func TestPLAN3555_ApplierPathContentConflictIsAContentConflict(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)
	conn, resp, err := dialCollab(t, ts.URL, item.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)
	waitForApplierPath(t, srv, slug, item.Slug, item.ID)

	read, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug
	rr := doRequest(srv, "PATCH", path, map[string]any{"expected_seq": read.Seq, "content": "first"})
	var ok struct {
		Warnings map[string]any `json:"warnings"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &ok)
	if rr.Code != http.StatusOK || ok.Warnings["content_outcome"] != contentOutcomeAppliedPendingFlush {
		t.Fatalf("premise: the first write must take the applier path; got %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "PATCH", path, map[string]any{"expected_seq": read.Seq, "content": "second, stale"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale seq: %d %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if env.Error.Details["content_write"] != true || !strings.Contains(env.Error.Message, "Do not resend your body") {
		t.Fatalf("applier-path content conflict took the field answer: %s", rr.Body.String())
	}
}
