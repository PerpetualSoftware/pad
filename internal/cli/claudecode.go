// Package cli — Claude Code session-shape helpers.
//
// This file is the pure-Go data layer for the `pad session shape` CLI
// command (IDEA-1491). It provides:
//
//   - Project-slug derivation from a working directory, matching the
//     scheme used by Claude Code under ~/.claude/projects/.
//   - JSONL streaming parser that pulls byte / line / message-count /
//     usage / metadata metrics off a session log.
//   - Resolver that walks the cascade env-var → cwd-slug → autodetect
//     to pick the JSONL for the current session.
//   - A small per-agent-version context-budget lookup table.
//
// No external dependencies. Tests live alongside in claudecode_test.go.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ClaudeCodeProjectSlug derives Claude Code's project-directory name
// from an absolute cwd. The rule, verified against ~/.claude/projects/
// listings on a live install: every '/' becomes '-' and every '.'
// becomes '-' as well, with a leading '-' on the result.
//
// Examples:
//
//	/home/dave/Dev/docapp        -> -home-dave-Dev-docapp
//	/home/dave/.clay/mates       -> -home-dave--clay-mates
//	/home/dave/claude            -> -home-dave-claude
//
// The function does NOT lowercase — letter case is preserved per the
// observed directory listing.
func ClaudeCodeProjectSlug(cwd string) string {
	r := strings.NewReplacer("/", "-", ".", "-")
	return r.Replace(cwd)
}

// ClaudeCodeProjectsDir returns ~/.claude/projects, honoring $HOME.
func ClaudeCodeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// SessionUsage is the token-accounting block lifted from the last
// assistant-message line of a JSONL.
type SessionUsage struct {
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	TotalPrompt   int64 `json:"total_prompt"`
}

// SessionMetrics is the parsed-from-disk view of a Claude Code session
// JSONL. Zero values mean "field never seen in the file."
type SessionMetrics struct {
	Bytes              int64
	Lines              int64
	SessionStartedAt   string
	LastActivityAt     string
	CWD                string
	GitBranch          string
	AgentVersion       string
	MessageCounts      map[string]int64
	ToolInvocations    int64
	Usage              *SessionUsage
	HasUsage           bool
	ElapsedWallSeconds int64
}

// parseJSONLLine is the subset of a line we care about. Fields are
// optional; the JSONL has many record types and we want the union.
type jsonlLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Version   string `json:"version"`
	Message   *struct {
		Usage *rawUsage `json:"usage"`
		// Content is left as a raw blob (not []json.RawMessage) so a
		// Claude Code schema variant that emits content as a string or
		// object doesn't fail the whole line's decode and lose its
		// type/timestamp/version/usage data. The tool-use scan
		// re-decodes Content as an array on a best-effort basis.
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ParseSessionJSONL reads a Claude Code session JSONL and returns the
// aggregated metrics. Lines that fail to parse are silently skipped
// (this matches Claude Code's own tolerance for partial writes / mixed
// schemas across versions); the line and byte totals still include
// them so the gross-size view stays accurate.
func ParseSessionJSONL(path string) (*SessionMetrics, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat session log: %w", err)
	}
	f, err := os.Open(path) //nolint:gosec // path is resolved from a controlled cascade
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}
	defer f.Close()

	m := &SessionMetrics{
		Bytes:         info.Size(),
		MessageCounts: map[string]int64{},
	}

	// bufio.Reader instead of bufio.Scanner: Scanner has a hard
	// per-line cap (MaxScanTokenSize, defaults 64 KiB, bumped to
	// 8 MiB before) and a single oversized record — e.g. an inline
	// file attachment — would kill the whole command with
	// "bufio.Scanner: token too long." ReadBytes('\n') has no length
	// cap; oversized lines still parse via json.Unmarshal since
	// encoding/json has no size limit either.
	reader := bufio.NewReaderSize(f, 64*1024)

	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			m.Lines++
			// Strip the trailing newline (and an optional \r) before
			// parsing. encoding/json is tolerant of trailing whitespace
			// but the strip keeps things tidy and matches the prior
			// Scanner behavior.
			trimmed := raw
			if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
				trimmed = trimmed[:n-1]
				if n2 := len(trimmed); n2 > 0 && trimmed[n2-1] == '\r' {
					trimmed = trimmed[:n2-1]
				}
			}
			if err := parseSessionLine(trimmed, m); err != nil {
				// unparseable line — still counted in lines/bytes.
				_ = err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read session log: %w", err)
		}
	}

	if m.SessionStartedAt != "" && m.LastActivityAt != "" {
		if a, errA := time.Parse(time.RFC3339, m.SessionStartedAt); errA == nil {
			if b, errB := time.Parse(time.RFC3339, m.LastActivityAt); errB == nil {
				m.ElapsedWallSeconds = int64(b.Sub(a).Seconds())
				if m.ElapsedWallSeconds < 0 {
					m.ElapsedWallSeconds = 0
				}
			}
		}
	}

	return m, nil
}

// parseSessionLine decodes one JSONL record and folds its fields into
// the running SessionMetrics. Returns an error only on decode failure;
// callers should treat that as "skip this line, keep going."
func parseSessionLine(raw []byte, m *SessionMetrics) error {
	if len(raw) == 0 {
		return nil
	}
	var line jsonlLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return err
	}

	// Message-count buckets. Anything outside the canonical
	// user/assistant pair collapses into "other" so the schema
	// stays stable across Claude Code releases.
	switch line.Type {
	case "user", "assistant":
		m.MessageCounts[line.Type]++
	default:
		m.MessageCounts["other"]++
	}

	if line.Timestamp != "" {
		if m.SessionStartedAt == "" {
			m.SessionStartedAt = line.Timestamp
		}
		m.LastActivityAt = line.Timestamp
	}
	if line.CWD != "" {
		m.CWD = line.CWD
	}
	if line.GitBranch != "" {
		m.GitBranch = line.GitBranch
	}
	if line.Version != "" {
		m.AgentVersion = line.Version
	}

	if line.Type == "assistant" && line.Message != nil {
		// tool_invocations: count every tool_use block — a single
		// assistant turn can issue multiple parallel tool calls
		// (multiple content[] entries with type=tool_use), and the
		// field name says invocations, not turns-with-any-tool-use.
		// Content may be array OR string OR object across Claude Code
		// schema variants; only the array shape carries tool_use
		// blocks. If the re-decode fails we skip tool counting but
		// keep the line's other data (already folded in above).
		if len(line.Message.Content) > 0 {
			var blocks []json.RawMessage
			if err := json.Unmarshal(line.Message.Content, &blocks); err == nil {
				for _, blob := range blocks {
					var head struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal(blob, &head); err == nil && head.Type == "tool_use" {
						m.ToolInvocations++
					}
				}
			}
		}
		if u := line.Message.Usage; u != nil {
			m.HasUsage = true
			m.Usage = &SessionUsage{
				CacheRead:     u.CacheReadInputTokens,
				CacheCreation: u.CacheCreationInputTokens,
				Input:         u.InputTokens,
				Output:        u.OutputTokens,
				TotalPrompt:   u.CacheReadInputTokens + u.CacheCreationInputTokens + u.InputTokens,
			}
		}
	}
	return nil
}

// ContextBudgetEntry maps an agent_version prefix to a context-window
// token budget.
type ContextBudgetEntry struct {
	Prefix string
	Budget int64
}

// DefaultContextBudgets is the seed table. Per IDEA-1491 recon: Claude
// Code 2.1.x on Claude 4.x has a 1M context window. Future agent
// versions get new rows.
//
// Match semantics: longest prefix wins. Keep the table sorted in
// descending prefix-length so the linear scan in LookupContextBudget
// hits specifics before generics.
var DefaultContextBudgets = []ContextBudgetEntry{
	{Prefix: "2.1.", Budget: 1_000_000},
}

// LookupContextBudget returns the matching budget for an agent_version
// string, or (0, false) when no prefix matches.
func LookupContextBudget(agentVersion string) (int64, bool) {
	for _, e := range DefaultContextBudgets {
		if strings.HasPrefix(agentVersion, e.Prefix) {
			return e.Budget, true
		}
	}
	return 0, false
}

// ContextClass buckets a percentage according to the IDEA-1491 design:
// <25% low, 25-55% moderate, 55-80% heavy, >80% dense.
func ContextClass(pct float64) string {
	switch {
	case pct < 25:
		return "low"
	case pct < 55:
		return "moderate"
	case pct < 80:
		return "heavy"
	default:
		return "dense"
	}
}

// ResolveOptions controls the session-resolver cascade.
type ResolveOptions struct {
	// ExplicitSession is the value of --session. It may be an absolute
	// path to a .jsonl file, or a session UUID to look up under
	// ~/.claude/projects.
	ExplicitSession string
	// CWD overrides os.Getwd() — primarily for tests.
	CWD string
}

// ResolveResult is the output of ResolveSessionLog.
type ResolveResult struct {
	Path      string
	SessionID string
	// Source describes which cascade tier matched, for debugging.
	// One of: "flag-path", "flag-id", "env-id", "autodetect".
	Source string
}

// ErrNoSession is returned when none of the cascade tiers locate a
// session log. Callers should switch to the IDEA-body sketch fallback.
var ErrNoSession = errors.New("claude code session log not found")

// ErrInvalidSessionID is returned when a session-ID-shaped input
// contains path separators, `..`, or other characters that would
// allow it to escape the projects-dir when joined as a filename
// fragment. Caller-facing — `pad session shape` surfaces it directly.
var ErrInvalidSessionID = errors.New("invalid session id")

// sessionIDPattern matches the UUID-ish shapes Claude Code emits for
// session IDs (e.g. "e8919313-e76d-420a-bffa-a4646d2d6a83"). We allow
// hex + dashes generously rather than pinning to canonical UUID
// length, because future agent versions may shift formats and the
// goal here is a defense-in-depth filename-safety check, not strict
// UUID enforcement.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{8,}$`)

// validateSessionID rejects values that would be unsafe to use as a
// filename fragment under ~/.claude/projects/<slug>/. Path separators,
// `..` segments, and anything that doesn't look UUID-ish trip this.
func validateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidSessionID)
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidSessionID, id)
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("%w: %q contains '..'", ErrInvalidSessionID, id)
	}
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q is not a UUID-shaped session id", ErrInvalidSessionID, id)
	}
	return nil
}

// ResolveSessionLog walks the cascade described in IDEA-1491 to locate
// the JSONL for the current session.
func ResolveSessionLog(opts ResolveOptions) (*ResolveResult, error) {
	cwd := opts.CWD
	if cwd == "" {
		c, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
		cwd = c
	}

	// Tier 1: --session flag.
	if opts.ExplicitSession != "" {
		// Absolute path?
		if filepath.IsAbs(opts.ExplicitSession) {
			if info, err := os.Stat(opts.ExplicitSession); err == nil && !info.IsDir() {
				return &ResolveResult{
					Path:      opts.ExplicitSession,
					SessionID: strings.TrimSuffix(filepath.Base(opts.ExplicitSession), ".jsonl"),
					Source:    "flag-path",
				}, nil
			}
			return nil, fmt.Errorf("--session path %q not found", opts.ExplicitSession)
		}
		// Treat as session UUID — validate shape before letting it
		// become a filename fragment under ~/.claude/projects/<slug>/.
		// Without this guard, `--session ../../../../../etc/foo` would
		// escape the projects dir on the os.Stat candidate-check.
		if err := validateSessionID(opts.ExplicitSession); err != nil {
			return nil, err
		}
		path, err := findSessionByID(opts.ExplicitSession)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Path: path, SessionID: opts.ExplicitSession, Source: "flag-id"}, nil
	}

	// Tier 2: $CLAUDE_CODE_SESSION_ID + cwd-derived slug.
	if id := os.Getenv("CLAUDE_CODE_SESSION_ID"); id != "" {
		// Same defense as the flag-id branch: env vars are
		// attacker-influenceable too (e.g. inherited from a parent
		// process or a sourced .env), so the ID must pass shape
		// validation before we join it into a filesystem path.
		if err := validateSessionID(id); err != nil {
			return nil, err
		}
		projectsDir, err := ClaudeCodeProjectsDir()
		if err != nil {
			return nil, err
		}
		slug := ClaudeCodeProjectSlug(cwd)
		candidate := filepath.Join(projectsDir, slug, id+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return &ResolveResult{Path: candidate, SessionID: id, Source: "env-id"}, nil
		}
		// Fall through — the env var may name a session in a different
		// project (e.g. when cwd has shifted mid-session).
		if path, err := findSessionByID(id); err == nil {
			return &ResolveResult{Path: path, SessionID: id, Source: "env-id"}, nil
		}
	}

	// Tier 3: autodetect when running inside Claude Code.
	if os.Getenv("CLAUDECODE") == "1" {
		path, id, err := autodetectSessionLog(cwd)
		if err == nil {
			return &ResolveResult{Path: path, SessionID: id, Source: "autodetect"}, nil
		}
	}

	return nil, ErrNoSession
}

func findSessionByID(id string) (string, error) {
	projectsDir, err := ClaudeCodeProjectsDir()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", fmt.Errorf("read projects dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, e.Name(), id+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("session %q: %w", id, ErrNoSession)
}

// autodetectSessionLog picks the most-recently-modified JSONL under
// the cwd-slug directory whose last-line cwd matches the live cwd.
// This is the "I'm running inside Claude Code but didn't get an env
// var" path.
func autodetectSessionLog(cwd string) (string, string, error) {
	projectsDir, err := ClaudeCodeProjectsDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(projectsDir, ClaudeCodeProjectSlug(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("read project slug dir %q: %w", dir, err)
	}

	type cand struct {
		path  string
		mtime time.Time
	}
	var candidates []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, cand{
			path:  filepath.Join(dir, e.Name()),
			mtime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return "", "", ErrNoSession
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].mtime.After(candidates[j].mtime)
	})

	for _, c := range candidates {
		if lastCWD := tailLineCWD(c.path); lastCWD == cwd {
			id := strings.TrimSuffix(filepath.Base(c.path), ".jsonl")
			return c.path, id, nil
		}
	}
	// No cwd match — fall back to most-recent.
	c := candidates[0]
	id := strings.TrimSuffix(filepath.Base(c.path), ".jsonl")
	return c.path, id, nil
}

// tailLineCWD reads the last parseable line of a JSONL and returns the
// cwd field (or ""). Used for autodetect's cwd match. Streams (no
// full-file read) so it stays cheap for multi-MB session logs.
func tailLineCWD(path string) string {
	f, err := os.Open(path) //nolint:gosec // path enumerated from projectsDir we control
	if err != nil {
		return ""
	}
	defer f.Close()
	// Use bufio.Reader (no per-line size cap) for parity with
	// ParseSessionJSONL — an oversized line shouldn't kill autodetect.
	reader := bufio.NewReaderSize(f, 64*1024)
	var lastCWD string
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			var line jsonlLine
			if err := json.Unmarshal(raw, &line); err == nil {
				if line.CWD != "" {
					lastCWD = line.CWD
				}
			}
		}
		if err != nil {
			break
		}
	}
	return lastCWD
}
