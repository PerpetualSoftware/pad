package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-4024: `pad item list --format json` with no --limit used to stop at the
// CLI's own 200 default and say so on stderr only, so a consumer parsing the
// array saw 200 of 204 rows and no signal. It now pages to completion.

// pagedItemServer is a fake of the list endpoints: total rows, at most
// serverPage per response, X-Pad-Has-More / X-Pad-Next-Offset like the real
// server. It records each request's raw query. Rows are ids "it-<n>".
type pagedItemServer struct {
	mu         sync.Mutex
	total      int
	serverPage int
	noHeaders  bool   // an older server: no paging state at all
	foreverAt  int    // if >0, always claim more (next offset = offset+foreverAt)
	jump       bool   // page one claims a next offset past its rows, then the end
	stuck      bool   // claim more but never advance the next offset
	undeclared string // X-Pad-Undeclared-Filter-Keys, first page only
	withCode   bool   // rows carry a code context (what reconcile keeps)
	queries    []string
}

func (p *pagedItemServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/items") {
			http.NotFound(w, r)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.queries = append(p.queries, r.URL.RawQuery)
		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		size := p.serverPage
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l < size {
			size = l
		}
		if p.noHeaders {
			size = p.total
		}
		to := min(from+size, p.total)
		if p.undeclared != "" && from == 0 {
			w.Header().Set("X-Pad-Undeclared-Filter-Keys", p.undeclared)
		}
		switch {
		case p.noHeaders:
		case p.jump:
			if from == 0 {
				w.Header().Set("X-Pad-Has-More", "true")
				w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to+size))
			} else {
				w.Header().Set("X-Pad-Has-More", "false")
			}
		case p.stuck:
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(from))
		case p.foreverAt > 0:
			to = from + p.foreverAt
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		case to < p.total:
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		default:
			w.Header().Set("X-Pad-Has-More", "false")
		}
		items := []models.Item{}
		for i := from; i < to; i++ {
			it := models.Item{ID: fmt.Sprintf("it-%d", i), Title: fmt.Sprintf("Item %d", i), CollectionSlug: "tasks"}
			if p.withCode {
				it.CodeContext = &models.ItemCodeContext{Provider: "github"}
			}
			items = append(items, it)
		}
		_ = json.NewEncoder(w).Encode(items)
	})
}

func (p *pagedItemServer) queryValues(t *testing.T) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

// runItemList runs `pad item list <args>` in the given format and returns
// stdout, stderr and the command's error.
func runItemList(t *testing.T, format string, args ...string) (string, string, error) {
	t.Helper()
	formatFlag = format
	cmd := listCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdoutDraining(t, func() { err = cmd.Execute() })
	})
	return stdout, stderr, err
}

// captureStdoutDraining is captureStdout for output bigger than a pipe buffer
// (64 KiB): a few thousand JSON rows block the writer if nothing reads until
// fn returns.
func captureStdoutDraining(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func decodeSummaries(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%.300s", err, out)
	}
	return rows
}

func TestBUG4024_JSONListPagesToCompletionWithoutALimit(t *testing.T) {
	p := &pagedItemServer{total: 2350, serverPage: 1000}
	setupFormatRoutingTest(t, p.handler())
	out, errOut, err := runItemList(t, "json")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 2350 {
		t.Fatalf("JSON array has %d rows, want all 2350", n)
	}
	for _, q := range p.queryValues(t) {
		if strings.Contains(q, "limit=") {
			t.Fatalf("auto-page sent a limit: %q", q)
		}
	}
	if got := len(p.queryValues(t)); got != 3 {
		t.Fatalf("%d requests, want 3 pages", got)
	}
	if strings.Contains(errOut, "more exist") || strings.Contains(errOut, "showing") {
		t.Fatalf("auto-page printed a truncation notice: %q", errOut)
	}
}

func TestBUG4024_JSONListAcrossTheDefaultPageBoundary(t *testing.T) {
	p := &pagedItemServer{total: 205, serverPage: 1000}
	setupFormatRoutingTest(t, p.handler())
	out, _, err := runItemList(t, "json")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 205 {
		t.Fatalf("got %d rows, want 205", n)
	}
}

func TestBUG4024_ExplicitLimitStaysCappedWithTheNotice(t *testing.T) {
	p := &pagedItemServer{total: 50, serverPage: 1000}
	setupFormatRoutingTest(t, p.handler())
	out, errOut, err := runItemList(t, "json", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 5 {
		t.Fatalf("got %d rows, want the explicit cap of 5", n)
	}
	if qs := p.queryValues(t); len(qs) != 1 || !strings.Contains(qs[0], "limit=5") {
		t.Fatalf("requests %q, want one with limit=5", qs)
	}
	if !strings.Contains(errOut, "more exist") {
		t.Fatalf("explicit --limit lost its stderr notice: %q", errOut)
	}
}

// What --limit 0 means today (BUG-4024 Q1), pinned: it is "the default", so
// the CLI clamps it to defaultItemListLimit (200) and sends limit=200, one
// page, with the notice. It is NOT "no limit" and does not auto-page.
func TestBUG4024_LimitZeroMeansTheDefaultCapNotUnlimited(t *testing.T) {
	p := &pagedItemServer{total: 450, serverPage: 1000}
	setupFormatRoutingTest(t, p.handler())
	out, errOut, err := runItemList(t, "json", "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != defaultItemListLimit {
		t.Fatalf("got %d rows, want the default cap %d", n, defaultItemListLimit)
	}
	if qs := p.queryValues(t); len(qs) != 1 || !strings.Contains(qs[0], "limit=200") {
		t.Fatalf("requests %q, want one with limit=200", qs)
	}
	if !strings.Contains(errOut, "more exist") {
		t.Fatalf("--limit 0 lost its stderr notice: %q", errOut)
	}
}

func TestBUG4024_OlderServerWithoutPagingHeadersIsOneRequest(t *testing.T) {
	p := &pagedItemServer{total: 7, serverPage: 3, noHeaders: true}
	setupFormatRoutingTest(t, p.handler())
	out, _, err := runItemList(t, "json")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 7 {
		t.Fatalf("got %d rows, want 7", n)
	}
	if qs := p.queryValues(t); len(qs) != 1 || strings.Contains(qs[0], "limit=") {
		t.Fatalf("requests %q, want one without a limit", qs)
	}
}

// An older server sends no paging state, and the unpaged notice falls back to
// "len == limit" as its guess. On the auto-page path there is no limit, so a
// list that happens to hold exactly 200 rows must not be reported as capped.
func TestBUG4024_AutoPageNeverPrintsTheCappedGuessOnAnOlderServer(t *testing.T) {
	p := &pagedItemServer{total: defaultItemListLimit, serverPage: 1000, noHeaders: true}
	setupFormatRoutingTest(t, p.handler())
	out, errOut, err := runItemList(t, "json")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != defaultItemListLimit {
		t.Fatalf("got %d rows, want %d", n, defaultItemListLimit)
	}
	if strings.Contains(errOut, "capped") || strings.Contains(errOut, "showing") {
		t.Fatalf("auto-page printed a truncation notice: %q", errOut)
	}
}

func TestBUG4024_OffsetWithoutALimitPagesFromThatOffsetToTheEnd(t *testing.T) {
	p := &pagedItemServer{total: 2350, serverPage: 1000}
	setupFormatRoutingTest(t, p.handler())
	out, _, err := runItemList(t, "json", "--offset", "1500")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 850 {
		t.Fatalf("got %d rows, want 850 (2350 - 1500)", len(rows))
	}
	if qs := p.queryValues(t); len(qs) != 1 || !strings.Contains(qs[0], "offset=1500") {
		t.Fatalf("requests %q, want one page starting at offset=1500", qs)
	}
}

func TestBUG4024_NonAdvancingOffsetIsAnErrorNotAShortArray(t *testing.T) {
	p := &pagedItemServer{total: 500, serverPage: 100, stuck: true}
	setupFormatRoutingTest(t, p.handler())
	out, _, err := runItemList(t, "json")
	if err == nil || !strings.Contains(err.Error(), "does not advance") {
		t.Fatalf("err = %v, want a does-not-advance error", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a partial array reached stdout: %.200s", out)
	}
}

func TestBUG4024_SkippingNextOffsetIsAnErrorNotAShortArray(t *testing.T) {
	p := &pagedItemServer{total: 500, serverPage: 100, jump: true}
	setupFormatRoutingTest(t, p.handler())
	out, _, err := runItemList(t, "json")
	if err == nil || !strings.Contains(err.Error(), "rather than return a partial list") {
		t.Fatalf("err = %v, want a partial-list refusal", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a partial array reached stdout: %.200s", out)
	}
}

func TestBUG4024_PageCapIsANonZeroExitNamingTheCapNeverAPartialArray(t *testing.T) {
	defer cli.SetMaxListPages(3)()
	p := &pagedItemServer{total: 1 << 30, serverPage: 10, foreverAt: 10}
	setupFormatRoutingTest(t, p.handler())
	out, _, err := runItemList(t, "json")
	if err == nil || !strings.Contains(err.Error(), "3-page cap") {
		t.Fatalf("err = %v, want an error naming the 3-page cap", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a partial array reached stdout: %.200s", out)
	}
	if got := len(p.queryValues(t)); got != 3 {
		t.Fatalf("%d requests, want exactly the cap of 3", got)
	}
}

func TestBUG4024_TableAndMarkdownKeepTheCapAndTheNotice(t *testing.T) {
	for _, format := range []string{"table", "markdown"} {
		t.Run(format, func(t *testing.T) {
			p := &pagedItemServer{total: 450, serverPage: 1000}
			setupFormatRoutingTest(t, p.handler())
			_, errOut, err := runItemList(t, format)
			if err != nil {
				t.Fatal(err)
			}
			if qs := p.queryValues(t); len(qs) != 1 || !strings.Contains(qs[0], "limit=200") {
				t.Fatalf("requests %q, want one with limit=200", qs)
			}
			if !strings.Contains(errOut, "more exist") {
				t.Fatalf("%s lost its stderr notice: %q", format, errOut)
			}
		})
	}
}

func TestBUG4024_UndeclaredKeyWarningSurvivesPaging(t *testing.T) {
	p := &pagedItemServer{total: 25, serverPage: 10, undeclared: "legacy_key"}
	setupFormatRoutingTest(t, p.handler())
	out, errOut, err := runItemList(t, "json")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 25 {
		t.Fatalf("got %d rows, want 25", n)
	}
	if strings.Count(errOut, "which no schema declares") != 1 || !strings.Contains(errOut, "legacy_key") {
		t.Fatalf("stderr %q, want the undeclared-key warning exactly once", errOut)
	}
}

func TestBUG4024_CollectionListPagesAndKeepsTheUndeclaredWarning(t *testing.T) {
	p := &pagedItemServer{total: 25, serverPage: 10, undeclared: "legacy_key"}
	setupFormatRoutingTest(t, p.handler())
	out, errOut, err := runItemList(t, "json", "tasks")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 25 {
		t.Fatalf("got %d rows, want 25", n)
	}
	if !strings.Contains(errOut, "legacy_key") {
		t.Fatalf("stderr %q lost the undeclared-key warning", errOut)
	}
}

// The alias fallback still works on the auto-page path: an older server that
// does not resolve `task` 404s it, and the retry as `tasks` pages.
func TestBUG4024_CollectionAliasFallbackStillPagesToCompletion(t *testing.T) {
	p := &pagedItemServer{total: 25, serverPage: 10}
	inner := p.handler()
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/server/capabilities":
			_, _ = w.Write([]byte(`{"collection_resolution":false}`))
		case strings.Contains(r.URL.Path, "/collections/task/items"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Collection not found"}}`))
		default:
			inner.ServeHTTP(w, r)
		}
	}))
	out, _, err := runItemList(t, "json", "task")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeSummaries(t, out)); n != 25 {
		t.Fatalf("got %d rows, want 25", n)
	}
}

// --- the existing ListAll* callers: normal multi-page input is unchanged ---
// (reconcile and github status are also covered by bug3793_reconcile_pages_test.go.)

func TestBUG4024_CallerReconcileReadsEveryPage(t *testing.T) {
	p := &pagedItemServer{total: 7, serverPage: 3, withCode: true}
	setupFormatRoutingTest(t, p.handler())
	got, err := reconcileTargetItems(cli.NewClientFromURL(urlFlag), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("got %d items, want 7", len(got))
	}
}

func TestBUG4024_CallerGitHubStatusScansEveryPage(t *testing.T) {
	p := &pagedItemServer{total: 7, serverPage: 3}
	inner := p.handler()
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/collections") {
			_ = json.NewEncoder(w).Encode([]models.Collection{{Slug: "tasks"}})
			return
		}
		inner.ServeHTTP(w, r)
	}))
	got, err := githubStatusScanItems(cli.NewClientFromURL(urlFlag), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("got %d items, want 7", len(got))
	}
}

func TestBUG4024_CallerStandupReadsEveryInProgressPage(t *testing.T) {
	p := &pagedItemServer{total: 7, serverPage: 3}
	inner := p.handler()
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/dashboard"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/collections"):
			_, _ = w.Write([]byte(`[]`))
		default:
			inner.ServeHTTP(w, r)
		}
	}))
	formatFlag = "json"
	cmd := standupCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(nil)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		InProgress []map[string]any `json:"in_progress"`
	}
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("standup output: %v\n%.300s", jerr, out)
	}
	if len(got.InProgress) != 7 {
		t.Fatalf("in_progress has %d items, want 7", len(got.InProgress))
	}
}

// --- a real server over the real store, on both backends ---
// The fake server above proves the CLI follows the headers it is told about.
// This proves the headers the real list endpoint sends are the ones the pager
// follows: 205 rows is one past the CLI's old 200 default.

func TestBUG4024_RealServerJSONListReturnsEveryRowOnBothBackends(t *testing.T) {
	backends := []struct {
		name string
		open func(*testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	}
	const total = 205
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			api := padserver.New(s)
			t.Cleanup(api.Stop)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Demo", Slug: "demo"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
				t.Fatal(err)
			}
			tasks, err := s.GetCollectionBySlug(ws.ID, "tasks")
			if err != nil || tasks == nil {
				t.Fatalf("tasks collection: %v", err)
			}
			for i := 0; i < total; i++ {
				if _, err := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Task %03d", i), Fields: `{"status":"open"}`}); err != nil {
					t.Fatal(err)
				}
			}
			setupFormatRoutingTest(t, api)

			out, errOut, err := runItemList(t, "json", "tasks")
			if err != nil {
				t.Fatal(err)
			}
			rows := decodeSummaries(t, out)
			if len(rows) != total {
				t.Fatalf("collection list: %d rows, want all %d (stderr %q)", len(rows), total, errOut)
			}
			seen := map[any]bool{}
			for _, r := range rows {
				seen[r["ref"]] = true
			}
			if len(seen) != total {
				t.Fatalf("collection list repeated rows: %d distinct refs of %d", len(seen), total)
			}

			out, _, err = runItemList(t, "json")
			if err != nil {
				t.Fatal(err)
			}
			// The workspace list also carries the template's seeded system
			// items, so it is at least the 205 tasks, each ref once.
			all := decodeSummaries(t, out)
			refs := map[any]bool{}
			for _, r := range all {
				refs[r["ref"]] = true
			}
			if len(all) < total || len(refs) != len(all) {
				t.Fatalf("workspace list: %d rows, %d distinct refs; want at least %d, no repeats", len(all), len(refs), total)
			}

			// An explicit --limit is still the caller's cap.
			out, errOut, err = runItemList(t, "json", "tasks", "--limit", "50")
			if err != nil {
				t.Fatal(err)
			}
			if n := len(decodeSummaries(t, out)); n != 50 || !strings.Contains(errOut, "more exist") {
				t.Fatalf("--limit 50: %d rows, stderr %q; want 50 rows and the notice", n, errOut)
			}
		})
	}
}
