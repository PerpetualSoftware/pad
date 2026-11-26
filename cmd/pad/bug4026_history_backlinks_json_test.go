package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4026: `pad item history` and `pad item backlinks` silently capped at 50
// under `--format json`. Both now follow the BUG-4024 contract: `--format json`
// with no explicit `--limit` returns the whole list as a plain JSON array,
// while an explicit `--limit` keeps bounded behaviour with a stderr notice.

type fakeHistoryServer struct {
	mu      sync.Mutex
	total   int
	queries []string
}

func (f *fakeHistoryServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/versions") {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.RawQuery)

		n := f.total
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 && l < n {
				n = l
			}
		}
		versions := make([]models.Version, 0, n)
		base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		for i := 0; i < n; i++ {
			versions = append(versions, models.Version{
				ID:            fmt.Sprintf("ver-%d", i),
				DocumentID:    "item-1",
				CreatedAt:     base.Add(-time.Duration(i) * time.Minute),
				CreatedBy:     "alice",
				Source:        "cli",
				ChangeSummary: fmt.Sprintf("edit %d", i),
			})
		}
		_ = json.NewEncoder(w).Encode(versions)
	})
}

func (f *fakeHistoryServer) queryValues(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func runItemHistory(t *testing.T, format string, args ...string) (string, string, error) {
	t.Helper()
	formatFlag = format
	cmd := historyCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdoutDraining(t, func() { err = cmd.Execute() })
	})
	return stdout, stderr, err
}

func TestBUG4026_HistoryJSONWithoutLimitReturnsAllRows(t *testing.T) {
	f := &fakeHistoryServer{total: 120}
	setupFormatRoutingTest(t, f.handler())

	out, errOut, err := runItemHistory(t, "json", "TASK-5")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 120 {
		t.Fatalf("got %d versions, want all 120", len(rows))
	}
	qs := f.queryValues(t)
	if len(qs) != 1 {
		t.Fatalf("got %d requests, want 1", len(qs))
	}
	if strings.Contains(qs[0], "limit=") {
		t.Fatalf("JSON without --limit sent limit query param: %q", qs[0])
	}
	if strings.TrimSpace(errOut) != "" {
		t.Fatalf("unexpected stderr notice: %q", errOut)
	}
}

func TestBUG4026_HistoryJSONWithExplicitLimitCapsAndWarnsOnStderr(t *testing.T) {
	f := &fakeHistoryServer{total: 120}
	setupFormatRoutingTest(t, f.handler())

	out, errOut, err := runItemHistory(t, "json", "TASK-5", "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 10 {
		t.Fatalf("got %d versions, want 10", len(rows))
	}
	qs := f.queryValues(t)
	if len(qs) != 1 || !strings.Contains(qs[0], "limit=11") {
		t.Fatalf("requests %q, want one with limit=11", qs)
	}
	wantNotice := "Showing the newest 10 versions; pass --limit 0 for all, or --limit M for more."
	if !strings.Contains(errOut, wantNotice) {
		t.Fatalf("stderr = %q, want %q", errOut, wantNotice)
	}
}

func TestBUG4026_HistoryTableWithoutLimitStillSendsLimit51(t *testing.T) {
	f := &fakeHistoryServer{total: 120}
	setupFormatRoutingTest(t, f.handler())

	out, _, err := runItemHistory(t, "table", "TASK-5")
	if err != nil {
		t.Fatal(err)
	}
	qs := f.queryValues(t)
	if len(qs) != 1 || !strings.Contains(qs[0], "limit=51") {
		t.Fatalf("requests %q, want one with limit=51", qs)
	}
	if !strings.Contains(out, "Showing the newest 50") {
		t.Fatalf("table stdout missing truncation line:\n%s", out)
	}
}

// --- backlinks ---

type pagedBacklinksServer struct {
	mu         sync.Mutex
	total      int
	serverPage int // max rows per response; defaults to 300 if 0
	noHeaders  bool
	short      bool // page 1 returns fewer rows than NextOffset implies
	jump       bool // page 1 claims NextOffset past its rows
	queries    []string
}

func (p *pagedBacklinksServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/items/TASK-5") {
			_ = json.NewEncoder(w).Encode(models.Item{
				ID:               "item-5",
				Slug:             "task-5-slug",
				Title:            "Target task",
				CollectionPrefix: "TASK",
			})
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/backlinks") {
			http.NotFound(w, r)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.queries = append(p.queries, r.URL.RawQuery)

		from, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		maxPage := p.serverPage
		if maxPage <= 0 {
			maxPage = 300
		}
		size := 50
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
				size = l
			}
		}
		if size > maxPage {
			size = maxPage
		}
		if p.noHeaders {
			size = min(size, p.total)
		}
		to := min(from+size, p.total)

		switch {
		case p.noHeaders:
			// Older server: no X-Pad-Has-More / X-Pad-Next-Offset headers.
		case p.short:
			if from == 0 {
				// Claims has-more with next offset = 300, but only returns 10 rows.
				to = min(from+10, p.total)
				w.Header().Set("X-Pad-Has-More", "true")
				w.Header().Set("X-Pad-Next-Offset", "300")
			} else {
				w.Header().Set("X-Pad-Has-More", "false")
			}
		case p.jump:
			if from == 0 {
				w.Header().Set("X-Pad-Has-More", "true")
				w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to+50))
			} else {
				w.Header().Set("X-Pad-Has-More", "false")
			}
		case to < p.total:
			w.Header().Set("X-Pad-Has-More", "true")
			w.Header().Set("X-Pad-Next-Offset", strconv.Itoa(to))
		default:
			w.Header().Set("X-Pad-Has-More", "false")
		}

		rows := make([]models.Backlink, 0, max(to-from, 0))
		for i := from; i < to; i++ {
			rows = append(rows, models.Backlink{
				SourceItemID: fmt.Sprintf("src-%d", i),
				SourceRef:    fmt.Sprintf("NOTE-%d", i+1),
				SourceTitle:  fmt.Sprintf("Source %d", i),
				Snippet:      fmt.Sprintf("See [[TASK-5]] (%d)", i),
			})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
}

func (p *pagedBacklinksServer) queryValues(t *testing.T) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

func runItemBacklinks(t *testing.T, format string, args ...string) (string, string, error) {
	t.Helper()
	formatFlag = format
	cmd := backlinksCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdoutDraining(t, func() { err = cmd.Execute() })
	})
	return stdout, stderr, err
}

func TestBUG4026_BacklinksJSONWithoutLimitPagesAll(t *testing.T) {
	p := &pagedBacklinksServer{total: 617, serverPage: 300}
	setupFormatRoutingTest(t, p.handler())

	out, errOut, err := runItemBacklinks(t, "json", "TASK-5")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 617 {
		t.Fatalf("got %d backlinks, want all 617", len(rows))
	}
	qs := p.queryValues(t)
	if len(qs) != 3 {
		t.Fatalf("got %d requests (%v), want 3 pages (300, 300, 17)", len(qs), qs)
	}
	if !strings.Contains(qs[0], "limit=300") || strings.Contains(qs[0], "offset=") {
		t.Fatalf("page 1 query = %q, want limit=300 and no offset", qs[0])
	}
	if !strings.Contains(qs[1], "limit=300") || !strings.Contains(qs[1], "offset=300") {
		t.Fatalf("page 2 query = %q, want limit=300&offset=300", qs[1])
	}
	if !strings.Contains(qs[2], "limit=300") || !strings.Contains(qs[2], "offset=600") {
		t.Fatalf("page 3 query = %q, want limit=300&offset=600", qs[2])
	}
	if strings.TrimSpace(errOut) != "" {
		t.Fatalf("unexpected stderr notice on auto-page: %q", errOut)
	}
}

func TestBUG4026_BacklinksJSONStartsFromOffset(t *testing.T) {
	p := &pagedBacklinksServer{total: 617, serverPage: 300}
	setupFormatRoutingTest(t, p.handler())

	out, _, err := runItemBacklinks(t, "json", "TASK-5", "--offset", "250")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 367 {
		t.Fatalf("got %d backlinks, want 367 (617 - 250)", len(rows))
	}
	qs := p.queryValues(t)
	if len(qs) != 2 {
		t.Fatalf("got %d requests (%v), want 2 pages", len(qs), qs)
	}
	if !strings.Contains(qs[0], "offset=250") || !strings.Contains(qs[1], "offset=550") {
		t.Fatalf("queries = %v, want offset=250 then offset=550", qs)
	}
}

func TestBUG4026_BacklinksJSONWithExplicitLimitCapsAndWarnsOnStderr(t *testing.T) {
	p := &pagedBacklinksServer{total: 25, serverPage: 300}
	setupFormatRoutingTest(t, p.handler())

	out, errOut, err := runItemBacklinks(t, "json", "TASK-5", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 5 {
		t.Fatalf("got %d backlinks, want 5", len(rows))
	}
	qs := p.queryValues(t)
	if len(qs) != 1 || !strings.Contains(qs[0], "limit=5") {
		t.Fatalf("requests %q, want one with limit=5", qs)
	}
	wantNotice := "Showing 5 backlinks; pass --offset 5 for more, or --format json with no --limit for all."
	if !strings.Contains(errOut, wantNotice) {
		t.Fatalf("stderr = %q, want %q", errOut, wantNotice)
	}
}

func TestBUG4026_BacklinksTableWarnsOnStderrWhenHasMore(t *testing.T) {
	p := &pagedBacklinksServer{total: 80, serverPage: 300}
	setupFormatRoutingTest(t, p.handler())

	_, errOut, err := runItemBacklinks(t, "table", "TASK-5")
	if err != nil {
		t.Fatal(err)
	}
	qs := p.queryValues(t)
	if len(qs) != 1 {
		t.Fatalf("requests %q, want 1", qs)
	}
	wantNotice := "Showing 50 backlinks; pass --offset 50 for more, or --format json with no --limit for all."
	if !strings.Contains(errOut, wantNotice) {
		t.Fatalf("stderr = %q, want %q", errOut, wantNotice)
	}
}

func TestBUG4026_BacklinksMalformedHasMorePageErrorsWithNoPartialOutput(t *testing.T) {
	for name, srv := range map[string]*pagedBacklinksServer{
		"short page":      {total: 617, serverPage: 300, short: true},
		"skipping offset": {total: 617, serverPage: 300, jump: true},
	} {
		t.Run(name, func(t *testing.T) {
			setupFormatRoutingTest(t, srv.handler())
			out, _, err := runItemBacklinks(t, "json", "TASK-5")
			if err == nil || !strings.Contains(err.Error(), "rather than return a partial list") {
				t.Fatalf("err = %v, want partial-list refusal", err)
			}
			if strings.TrimSpace(out) != "" {
				t.Fatalf("partial output reached stdout: %.200s", out)
			}
		})
	}
}

func TestBUG4026_BacklinksOlderServerWithoutHeaderReturnsSinglePage(t *testing.T) {
	p := &pagedBacklinksServer{total: 12, serverPage: 300, noHeaders: true}
	setupFormatRoutingTest(t, p.handler())

	out, errOut, err := runItemBacklinks(t, "json", "TASK-5")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeSummaries(t, out)
	if len(rows) != 12 {
		t.Fatalf("got %d backlinks, want 12", len(rows))
	}
	if qs := p.queryValues(t); len(qs) != 1 {
		t.Fatalf("got %d requests, want 1", len(qs))
	}
	if strings.TrimSpace(errOut) != "" {
		t.Fatalf("unexpected stderr: %q", errOut)
	}
}

// A server older than BUG-4026 sends no paging headers but still honours
// limit and offset. Its backlinks were always bounded, so a full header-less
// page must not be read as the whole list: the CLI keeps asking until a short
// page comes back.
func TestBUG4026_BacklinksOlderServerFullPagesKeepPaging(t *testing.T) {
	for _, tc := range []struct {
		name     string
		total    int
		requests int
	}{
		{"several pages", 617, 3},
		{"exactly one full page", 300, 2},
		{"empty", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &pagedBacklinksServer{total: tc.total, serverPage: 300, noHeaders: true}
			setupFormatRoutingTest(t, p.handler())

			out, errOut, err := runItemBacklinks(t, "json", "TASK-5")
			if err != nil {
				t.Fatal(err)
			}
			rows := decodeSummaries(t, out)
			if len(rows) != tc.total {
				t.Fatalf("got %d backlinks, want all %d", len(rows), tc.total)
			}
			if qs := p.queryValues(t); len(qs) != tc.requests {
				t.Fatalf("got %d requests (%v), want %d", len(qs), qs, tc.requests)
			}
			if strings.TrimSpace(errOut) != "" {
				t.Fatalf("unexpected stderr: %q", errOut)
			}
		})
	}
}
