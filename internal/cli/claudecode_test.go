package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeCodeProjectSlug(t *testing.T) {
	cases := []struct {
		cwd  string
		want string
	}{
		// Live-directory-listing-verified rules.
		{"/home/dave/Dev/docapp", "-home-dave-Dev-docapp"},
		{"/home/dave/claude", "-home-dave-claude"},
		{"/home/dave/.clay/mates", "-home-dave--clay-mates"},
		{"/home/dave/Dev/docapp-session-shape", "-home-dave-Dev-docapp-session-shape"},
		{"/", "-"},
	}
	for _, c := range cases {
		t.Run(c.cwd, func(t *testing.T) {
			got := ClaudeCodeProjectSlug(c.cwd)
			if got != c.want {
				t.Fatalf("slug(%q) = %q, want %q", c.cwd, got, c.want)
			}
		})
	}
}

func TestParseSessionJSONL_Normal(t *testing.T) {
	m, err := ParseSessionJSONL("testdata/sessions/normal.jsonl")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Lines != 8 {
		t.Errorf("Lines = %d, want 8", m.Lines)
	}
	if m.MessageCounts["user"] != 2 {
		t.Errorf("user count = %d, want 2", m.MessageCounts["user"])
	}
	if m.MessageCounts["assistant"] != 3 {
		t.Errorf("assistant count = %d, want 3", m.MessageCounts["assistant"])
	}
	if m.MessageCounts["other"] != 3 {
		t.Errorf("other count = %d, want 3 (custom-title + attachment + queue-operation)", m.MessageCounts["other"])
	}
	// One tool_use in turn 2, three parallel tool_use blocks in the
	// final assistant turn = 4 invocations. Pins the multi-tool-per-turn
	// counting behavior fixed in Codex R1 P2.
	if m.ToolInvocations != 4 {
		t.Errorf("tool_invocations = %d, want 4", m.ToolInvocations)
	}
	if m.AgentVersion != "2.1.132" {
		t.Errorf("AgentVersion = %q, want 2.1.132", m.AgentVersion)
	}
	if m.CWD != "/home/test/proj" {
		t.Errorf("CWD = %q", m.CWD)
	}
	if m.GitBranch != "main" {
		t.Errorf("GitBranch = %q", m.GitBranch)
	}
	if !m.HasUsage || m.Usage == nil {
		t.Fatalf("expected usage on last assistant line")
	}
	// Last assistant line's usage wins.
	if m.Usage.CacheRead != 12345 {
		t.Errorf("Usage.CacheRead = %d, want 12345", m.Usage.CacheRead)
	}
	if m.Usage.TotalPrompt != 12345+50+1 {
		t.Errorf("TotalPrompt = %d, want %d", m.Usage.TotalPrompt, 12345+50+1)
	}
	if m.SessionStartedAt != "2026-05-16T10:00:00Z" {
		t.Errorf("SessionStartedAt = %q", m.SessionStartedAt)
	}
	if m.LastActivityAt != "2026-05-16T10:02:00Z" {
		t.Errorf("LastActivityAt = %q", m.LastActivityAt)
	}
	if m.ElapsedWallSeconds != 120 {
		t.Errorf("ElapsedWallSeconds = %d, want 120", m.ElapsedWallSeconds)
	}
}

func TestParseSessionJSONL_NoUsage(t *testing.T) {
	m, err := ParseSessionJSONL("testdata/sessions/no_usage.jsonl")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.HasUsage {
		t.Error("expected HasUsage=false")
	}
	if m.Usage != nil {
		t.Error("expected nil Usage")
	}
	if m.Lines != 2 {
		t.Errorf("Lines = %d, want 2", m.Lines)
	}
}

func TestParseSessionJSONL_Sidechain(t *testing.T) {
	// v1: sidechain lines are still counted in totals (not separately
	// segregated). The parent's last-usage wins regardless of
	// isSidechain ordering — but here the parent's assistant line is
	// last, so it should win.
	m, err := ParseSessionJSONL("testdata/sessions/sidechain.jsonl")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Lines != 3 {
		t.Errorf("Lines = %d, want 3", m.Lines)
	}
	if m.Usage == nil || m.Usage.CacheRead != 3000 {
		t.Fatalf("expected parent's CacheRead=3000, got %+v", m.Usage)
	}
}

func TestLookupContextBudget(t *testing.T) {
	cases := []struct {
		ver  string
		want int64
		ok   bool
	}{
		{"2.1.132", 1_000_000, true},
		{"2.1.200", 1_000_000, true},
		{"2.0.99", 0, false},
		{"", 0, false},
		{"3.0.0", 0, false},
	}
	for _, c := range cases {
		t.Run(c.ver, func(t *testing.T) {
			got, ok := LookupContextBudget(c.ver)
			if ok != c.ok || got != c.want {
				t.Fatalf("LookupContextBudget(%q) = (%d,%v), want (%d,%v)", c.ver, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestContextClass(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, "low"},
		{24.9, "low"},
		{25, "moderate"},
		{54.9, "moderate"},
		{55, "heavy"},
		{79.9, "heavy"},
		{80, "dense"},
		{99.9, "dense"},
	}
	for _, c := range cases {
		if got := ContextClass(c.pct); got != c.want {
			t.Errorf("ContextClass(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

func TestResolveSessionLog_ExplicitPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "abc.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSessionLog(ResolveOptions{ExplicitSession: p})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != p {
		t.Errorf("Path = %q, want %q", got.Path, p)
	}
	if got.Source != "flag-path" {
		t.Errorf("Source = %q, want flag-path", got.Source)
	}
	if got.SessionID != "abc" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
}

func TestResolveSessionLog_EnvID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "feedface")
	t.Setenv("CLAUDECODE", "0")

	cwd := "/work/myproj"
	slug := ClaudeCodeProjectSlug(cwd)
	projDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(projDir, "feedface.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSessionLog(ResolveOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != logPath {
		t.Errorf("Path = %q, want %q", got.Path, logPath)
	}
	if got.Source != "env-id" {
		t.Errorf("Source = %q, want env-id", got.Source)
	}
}

func TestResolveSessionLog_AutodetectByCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDECODE", "1")

	cwd := "/work/auto"
	slug := ClaudeCodeProjectSlug(cwd)
	projDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Two candidate logs — older one matches cwd; newer one does NOT.
	// We expect the cwd-matching one to be picked even though it has
	// an older mtime.
	matchPath := filepath.Join(projDir, "match.jsonl")
	mismatchPath := filepath.Join(projDir, "mismatch.jsonl")
	if err := os.WriteFile(matchPath, []byte(`{"type":"user","cwd":"`+cwd+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatchPath, []byte(`{"type":"user","cwd":"/somewhere/else"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make mismatch newer.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(mismatchPath, future, future); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSessionLog(ResolveOptions{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != matchPath {
		t.Errorf("Path = %q, want %q", got.Path, matchPath)
	}
	if got.Source != "autodetect" {
		t.Errorf("Source = %q, want autodetect", got.Source)
	}
}

func TestResolveSessionLog_FallbackOnMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDECODE", "0")

	_, err := ResolveSessionLog(ResolveOptions{CWD: "/nope"})
	if err == nil {
		t.Fatal("expected ErrNoSession")
	}
}

// TestResolveSessionLog_RejectsPathTraversal pins the Codex R2 P1 fix:
// session-ID-shaped input that contains '..', '/', '\\', or doesn't
// match the UUID-ish shape is rejected before it can become a
// filename fragment under ~/.claude/projects/<slug>/.
func TestResolveSessionLog_RejectsPathTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDECODE", "0")

	bad := []string{
		"../../../../../tmp/foo",
		"../escape",
		"foo/bar",
		`foo\bar`,
		"..",
		".",
		"",
		"not-a-uuid!",
		"abc..def",
	}
	for _, id := range bad {
		t.Run("flag-id/"+id, func(t *testing.T) {
			_, err := ResolveSessionLog(ResolveOptions{ExplicitSession: id, CWD: "/work"})
			if err == nil {
				t.Fatalf("expected error for --session %q", id)
			}
			if id != "" && !errors.Is(err, ErrInvalidSessionID) {
				t.Fatalf("err = %v, want ErrInvalidSessionID", err)
			}
		})
		if id == "" {
			// Env-var case: empty string is treated as "not set" and
			// would fall through to autodetect, not validation.
			continue
		}
		t.Run("env-id/"+id, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", id)
			_, err := ResolveSessionLog(ResolveOptions{CWD: "/work"})
			if err == nil {
				t.Fatalf("expected error for env %q", id)
			}
			if !errors.Is(err, ErrInvalidSessionID) {
				t.Fatalf("err = %v, want ErrInvalidSessionID", err)
			}
		})
	}
}

// TestParseSessionJSONL_OversizedLine pins the Codex R2 P2-Scanner fix:
// a single record larger than the old bufio.Scanner cap (8 MiB) must
// parse cleanly instead of failing the whole command with
// "bufio.Scanner: token too long."
func TestParseSessionJSONL_OversizedLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.jsonl")
	// 10 MiB of inline-attachment-shaped content. Building the JSON by
	// hand to avoid any encoding-overhead surprise; the text inside the
	// string is benign ASCII so JSON quoting stays minimal.
	huge := strings.Repeat("x", 10*1024*1024)
	body := `{"type":"assistant","timestamp":"2026-05-16T00:00:00Z","version":"2.1.132","message":{"content":[{"type":"text","text":"` + huge + `"}],"usage":{"input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}}}` + "\n"
	body += `{"type":"user","timestamp":"2026-05-16T00:00:01Z","message":{"content":[{"type":"text","text":"after"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ParseSessionJSONL(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Lines != 2 {
		t.Errorf("Lines = %d, want 2", m.Lines)
	}
	if !m.HasUsage || m.Usage == nil || m.Usage.CacheRead != 3 {
		t.Errorf("expected usage with cache_read=3, got %+v", m.Usage)
	}
	if m.MessageCounts["assistant"] != 1 || m.MessageCounts["user"] != 1 {
		t.Errorf("message counts = %v", m.MessageCounts)
	}
}

// TestParseSessionJSONL_NonArrayContent pins the Codex R2 P2-content
// fix: a line whose message.content is a string (or any non-array)
// must still contribute its type/timestamp/version/usage to the
// metrics — only the tool_use scan is skipped.
func TestParseSessionJSONL_NonArrayContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "variant.jsonl")
	body := `{"type":"assistant","timestamp":"2026-05-16T00:00:00Z","version":"2.1.999","message":{"content":"just a string","usage":{"input_tokens":7,"cache_creation_input_tokens":11,"cache_read_input_tokens":13,"output_tokens":17}}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ParseSessionJSONL(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.MessageCounts["assistant"] != 1 {
		t.Errorf("expected assistant count 1, got %v", m.MessageCounts)
	}
	if m.AgentVersion != "2.1.999" {
		t.Errorf("AgentVersion = %q, want 2.1.999", m.AgentVersion)
	}
	if !m.HasUsage || m.Usage == nil || m.Usage.CacheRead != 13 {
		t.Errorf("expected usage with cache_read=13, got %+v", m.Usage)
	}
	if m.ToolInvocations != 0 {
		t.Errorf("ToolInvocations = %d, want 0 (non-array content)", m.ToolInvocations)
	}
}

// TestParseSessionJSONL_ParallelToolUse pins the Codex R1 P2 fix.
// (Already exercised via normal.jsonl, repeated here as a tight,
// hermetic check with a single line.)
func TestParseSessionJSONL_ParallelToolUse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "parallel.jsonl")
	body := `{"type":"assistant","timestamp":"2026-05-16T00:00:00Z","version":"2.1.132","message":{"content":[{"type":"tool_use","name":"a"},{"type":"tool_use","name":"b"},{"type":"tool_use","name":"c"},{"type":"text","text":"k"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ParseSessionJSONL(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ToolInvocations != 3 {
		t.Errorf("ToolInvocations = %d, want 3", m.ToolInvocations)
	}
}
