package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// MCPServerKey is the canonical name pad registers itself under in
// each agent's `mcpServers` map. Stable across versions; renaming
// would orphan every existing install.
const MCPServerKey = "pad"

// codexServersKey is the table key Codex nests its MCP servers under
// in ~/.codex/config.toml — `[mcp_servers.pad]`, snake_case, distinct
// from the JSON clients' camelCase `mcpServers`.
const codexServersKey = "mcp_servers"

// configFormat selects how an agent's config file is encoded. The JSON
// clients (Claude Desktop, Cursor, Windsurf, Claude Code) share one
// reader/writer; Codex uses TOML, which needs its own load/merge/write
// path (a JSON writer would corrupt a TOML file).
type configFormat int

const (
	formatJSON configFormat = iota
	formatTOML
)

// Agent describes a known MCP-compatible client and how to find its
// per-user config file.
type Agent struct {
	// Name is the canonical lookup key, used as the command argument
	// (e.g. `pad mcp install claude-desktop`).
	Name string
	// Label is the human-readable name for prints.
	Label string
	// Aliases match alternate user inputs.
	Aliases []string
	// Format selects the config file encoding (JSON vs TOML). Defaults
	// to formatJSON (the zero value) for the JSON clients.
	Format configFormat
	// CWDBased marks agents whose config lives in the current working
	// directory rather than under $HOME — Claude Code's project-local
	// `.mcp.json`. These are install-on-request only: they're excluded
	// from `--all` and from Status(), because "the current directory"
	// isn't a stable global target the way a per-user config file is
	// (sweeping a project-local file into whatever directory `--all`
	// happens to run from would scatter stray configs). See globalAgents.
	CWDBased bool
	// PathFor resolves the config path given a base directory and
	// runtime.GOOS. The base is the user's home directory for normal
	// agents, or the current working directory for CWDBased agents
	// (Installer.ResolvePath picks which). Pure function — no I/O — so
	// tests override `base` and `goos` to verify path logic without
	// touching the real filesystem.
	PathFor func(base, goos string) (string, error)
}

// SupportedAgents is the canonical list of MCP-aware clients pad
// knows how to configure. Order is the iteration order for the
// auto-detect / status path.
var SupportedAgents = []Agent{
	{
		Name:    "claude-desktop",
		Label:   "Claude Desktop",
		Aliases: []string{"claude", "anthropic"},
		PathFor: claudeDesktopPathFor,
	},
	{
		Name:    "cursor",
		Label:   "Cursor",
		PathFor: cursorPathFor,
	},
	{
		Name:    "windsurf",
		Label:   "Windsurf",
		PathFor: windsurfPathFor,
	},
	{
		Name:     "claude-code",
		Label:    "Claude Code",
		Aliases:  []string{"claudecode"},
		CWDBased: true, // project-local .mcp.json in the current directory
		PathFor:  claudeCodePathFor,
	},
	{
		Name:    "codex",
		Label:   "Codex",
		Format:  formatTOML,
		PathFor: codexPathFor,
	},
}

// globalAgents returns the agents that target a stable per-user config
// file — i.e. everything except CWDBased (project-local) agents. It's
// the iteration set for `--all` and Status(); CWDBased agents like
// Claude Code are install-on-request only (see Agent.CWDBased).
func globalAgents() []*Agent {
	out := make([]*Agent, 0, len(SupportedAgents))
	for i := range SupportedAgents {
		if SupportedAgents[i].CWDBased {
			continue
		}
		out = append(out, &SupportedAgents[i])
	}
	return out
}

// FindAgent returns the matching Agent for a name or alias. Names are
// case-insensitive (Claude / claude / CLAUDE all resolve).
func FindAgent(name string) (*Agent, error) {
	for i := range SupportedAgents {
		a := &SupportedAgents[i]
		if equalFold(a.Name, name) {
			return a, nil
		}
		for _, alias := range a.Aliases {
			if equalFold(alias, name) {
				return a, nil
			}
		}
	}
	names := make([]string, 0, len(SupportedAgents))
	for i := range SupportedAgents {
		names = append(names, SupportedAgents[i].Name)
	}
	return nil, fmt.Errorf("unknown agent %q (supported: %s)", name, strings.Join(names, ", "))
}

// equalFold is a tiny case-insensitive string compare without
// pulling in strings.EqualFold's full Unicode logic — agent names
// are ASCII.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func claudeDesktopPathFor(home, goos string) (string, error) {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA env var not set; cannot resolve Claude Desktop config path")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	default: // linux + bsd + others
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

func cursorPathFor(home, _ string) (string, error) {
	// Cursor uses ~/.cursor/mcp.json on every platform.
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

func windsurfPathFor(home, goos string) (string, error) {
	if goos == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA env var not set; cannot resolve Windsurf config path")
		}
		return filepath.Join(appData, "Codeium", "windsurf", "mcp_config.json"), nil
	}
	return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), nil
}

// claudeCodePathFor resolves Claude Code's project-local `.mcp.json`.
// `base` is the current working directory (Installer.ResolvePath passes
// cwd for CWDBased agents, not home) — Claude Code reads `.mcp.json`
// from the project root it's launched in, same file shape as the other
// JSON clients but scoped to a directory rather than a user.
func claudeCodePathFor(base, _ string) (string, error) {
	return filepath.Join(base, ".mcp.json"), nil
}

// codexPathFor resolves Codex's `~/.codex/config.toml` (same location on
// every platform — Codex is a CLI tool with a Unix-style dotdir).
func codexPathFor(home, _ string) (string, error) {
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// AddPadEntry reads (or creates) the JSON config at path, ensures
// mcpServers.pad points to `binary`, and writes the file back.
// Existing entries outside `mcpServers.pad` are preserved.
//
// Returns (modified=true, nil) when the on-disk content actually
// changed, (false, nil) when the file was already up to date.
func AddPadEntry(path, binary string) (bool, error) {
	if binary == "" {
		return false, errors.New("AddPadEntry: binary path is required")
	}
	cfg, err := loadJSONConfig(path)
	if err != nil {
		return false, err
	}
	wantedEntry := map[string]any{
		"command": binary,
		"args":    []any{"mcp", "serve"},
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	current, hadPad := servers[MCPServerKey].(map[string]any)
	if hadPad && jsonEqual(current, wantedEntry) {
		// Idempotent path: content already correct, no rewrite.
		// Still tighten perms — a previous tool / manual edit may
		// have left the file 0644, and the security-tightening
		// guarantee should not depend on whether content changed.
		// Codex round 2 (TASK-948) caught this gap.
		tightenPerms(path)
		return false, nil
	}
	servers[MCPServerKey] = wantedEntry
	cfg["mcpServers"] = servers
	if err := writeJSONConfig(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// tightenPerms sets path to 0600 if it exists. Best-effort: errors
// only emit a warning to stderr (the user's primary intent — install /
// no-op — already succeeded). Used by both the write-modified and
// idempotent code paths so the security claim ("config is 0600 after
// install") holds regardless of whether content changed.
func tightenPerms(path string) {
	if _, err := os.Stat(path); err != nil {
		return // nothing to tighten
	}
	if err := os.Chmod(path, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to chmod 0600 %s: %v\n", path, err)
	}
}

// RemovePadEntry deletes mcpServers.pad while leaving other servers +
// top-level keys intact. Returns (true, nil) when an entry was
// removed, (false, nil) when there was nothing to do (file missing,
// no mcpServers key, or no `pad` entry).
func RemovePadEntry(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	cfg, err := loadJSONConfig(path)
	if err != nil {
		return false, err
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, exists := servers[MCPServerKey]; !exists {
		return false, nil
	}
	delete(servers, MCPServerKey)
	cfg["mcpServers"] = servers
	if err := writeJSONConfig(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// HasPadEntry returns whether mcpServers.pad is present, plus the
// current `command` value (empty string when missing). Missing files
// are reported as not installed without error.
func HasPadEntry(path string) (installed bool, command string, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("stat %s: %w", path, statErr)
	}
	cfg, err := loadJSONConfig(path)
	if err != nil {
		return false, "", err
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		return false, "", nil
	}
	entry, ok := servers[MCPServerKey].(map[string]any)
	if !ok {
		return false, "", nil
	}
	cmd, _ := entry["command"].(string)
	return true, cmd, nil
}

// loadJSONConfig reads path as a JSON object. A missing or
// whitespace-only file is treated as an empty config (so AddPadEntry
// can populate from scratch on first install). Callers that need to
// distinguish "missing" from "empty" check os.Stat separately.
//
// Read or parse errors on a non-empty file propagate verbatim — we
// never silently overwrite a corrupt config; the user has to fix or
// remove it.
func loadJSONConfig(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if isAllWhitespace(b) {
		return map[string]any{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

func isAllWhitespace(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

func writeJSONConfig(path string, cfg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// 0o600 — config files often hold credentials for OTHER MCP
	// servers (postgres URIs, OpenAI keys, etc.); tighten perms even
	// though pad's own entry has no secrets.
	//
	// os.WriteFile only honors the mode when CREATING the file — an
	// existing 0644 config keeps 0644 after the write. Codex caught
	// this on TASK-948 round 1, so we Chmod after writing. Best-effort:
	// chmod failures don't fail the install (the data write
	// succeeded; the user can re-tighten manually).
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tightenPerms(path)
	return nil
}

// jsonEqual compares two parsed-JSON maps for value equality. Cheaper
// than reflect.DeepEqual and accepts the float/string nuances of
// json.Unmarshal output.
func jsonEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if len(ab) == 0 || len(bb) == 0 {
		return false
	}
	// Cheap-but-correct: round-trip both and compare the canonical
	// JSON byte form. Order is deterministic per encoding/json.
	return string(ab) == string(bb)
}

// --- TOML config path (Codex) --------------------------------------------
//
// Codex nests its MCP servers in a TOML `[mcp_servers.pad]` table, so the
// JSON reader/writer above won't do. These mirror the JSON functions'
// behaviour (merge, don't clobber; idempotent; 0600 perms) against a
// map[string]any decoded from / encoded to TOML.

// addPadEntryTOML reads (or creates) the TOML config at path, ensures
// mcp_servers.pad points to `binary`, and writes the file back. Existing
// unrelated keys and other mcp_servers entries are preserved. Returns
// (modified=true, nil) only when the on-disk content actually changed.
func addPadEntryTOML(path, binary string) (bool, error) {
	if binary == "" {
		return false, errors.New("addPadEntryTOML: binary path is required")
	}
	cfg, err := loadTOMLConfig(path)
	if err != nil {
		return false, err
	}
	wantedEntry := map[string]any{
		"command": binary,
		"args":    []any{"mcp", "serve"},
	}
	// If mcp_servers exists but isn't a table (e.g. a scalar left by a
	// hand-edit or an incompatible tool), refuse rather than silently
	// clobbering it — same "never overwrite a config we can't reconcile"
	// posture as loadTOMLConfig's parse-error path.
	rawServers, hasServers := cfg[codexServersKey]
	servers, ok := rawServers.(map[string]any)
	if hasServers && !ok {
		return false, fmt.Errorf("%s: %q exists but is not a table; refusing to overwrite", path, codexServersKey)
	}
	if servers == nil {
		servers = map[string]any{}
	}
	current, hadPad := servers[MCPServerKey].(map[string]any)
	if hadPad && jsonEqual(current, wantedEntry) {
		// Idempotent path: content already correct. Still tighten perms
		// (mirrors AddPadEntry) — a hand-edited config may be 0644.
		tightenPerms(path)
		return false, nil
	}
	servers[MCPServerKey] = wantedEntry
	cfg[codexServersKey] = servers
	if err := writeTOMLConfig(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// removePadEntryTOML deletes mcp_servers.pad while leaving other servers
// and top-level keys intact. Returns (true, nil) when an entry was
// removed, (false, nil) when there was nothing to do.
func removePadEntryTOML(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	cfg, err := loadTOMLConfig(path)
	if err != nil {
		return false, err
	}
	servers, ok := cfg[codexServersKey].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, exists := servers[MCPServerKey]; !exists {
		return false, nil
	}
	delete(servers, MCPServerKey)
	cfg[codexServersKey] = servers
	if err := writeTOMLConfig(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// hasPadEntryTOML reports whether mcp_servers.pad is present plus its
// current `command` value. Missing files are "not installed", no error.
func hasPadEntryTOML(path string) (installed bool, command string, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("stat %s: %w", path, statErr)
	}
	cfg, err := loadTOMLConfig(path)
	if err != nil {
		return false, "", err
	}
	servers, _ := cfg[codexServersKey].(map[string]any)
	if servers == nil {
		return false, "", nil
	}
	entry, ok := servers[MCPServerKey].(map[string]any)
	if !ok {
		return false, "", nil
	}
	cmd, _ := entry["command"].(string)
	return true, cmd, nil
}

// loadTOMLConfig reads path as a TOML document into a map. A missing or
// whitespace-only file is an empty config. Parse errors on a non-empty
// file propagate — we never silently overwrite a config we can't read.
func loadTOMLConfig(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if isAllWhitespace(b) {
		return map[string]any{}, nil
	}
	var raw map[string]any
	if err := toml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

func writeTOMLConfig(path string, cfg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal toml config: %w", err)
	}
	// 0o600 — like the JSON path, Codex configs can hold credentials for
	// other MCP servers. os.WriteFile only honors mode on create, so we
	// Chmod after (tightenPerms) to catch a pre-existing 0644 file.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tightenPerms(path)
	return nil
}

// --- format dispatch ------------------------------------------------------
//
// Install/Uninstall/Status route through these so each agent's config is
// read and written in its native format. JSON is the default; TOML is
// Codex-only for now.

func addEntry(agent *Agent, path, binary string) (bool, error) {
	if agent.Format == formatTOML {
		return addPadEntryTOML(path, binary)
	}
	return AddPadEntry(path, binary)
}

func removeEntry(agent *Agent, path string) (bool, error) {
	if agent.Format == formatTOML {
		return removePadEntryTOML(path)
	}
	return RemovePadEntry(path)
}

func hasEntry(agent *Agent, path string) (bool, string, error) {
	if agent.Format == formatTOML {
		return hasPadEntryTOML(path)
	}
	return HasPadEntry(path)
}

// Installer is the high-level façade over AddPadEntry / RemovePadEntry,
// resolving config paths from per-agent rules and the user's home dir.
//
// Production callers leave Home/GOOS empty so the installer queries
// the runtime; tests inject explicit values to point at a tempdir.
type Installer struct {
	// Binary is the pad executable to register. Required for Install.
	Binary string
	// Home overrides os.UserHomeDir when non-empty (test-only).
	Home string
	// CWD overrides os.Getwd when non-empty (test-only). Used to resolve
	// CWDBased agents' project-local config (Claude Code's .mcp.json).
	CWD string
	// GOOS overrides runtime.GOOS when non-empty (test-only).
	GOOS string
}

// AgentStatus is one row of Installer.Status output.
type AgentStatus struct {
	Name       string
	Label      string
	ConfigPath string // empty when path resolution failed
	Installed  bool
	Command    string // current `command` value, when installed
	// Error captures any non-fatal issue (path resolution failed,
	// config file unreadable). Status keeps reporting on success of
	// other agents even if one row errors.
	Error string
}

func (i *Installer) home() (string, error) {
	if i.Home != "" {
		return i.Home, nil
	}
	return os.UserHomeDir()
}

func (i *Installer) goos() string {
	if i.GOOS != "" {
		return i.GOOS
	}
	return runtime.GOOS
}

func (i *Installer) cwd() (string, error) {
	if i.CWD != "" {
		return i.CWD, nil
	}
	return os.Getwd()
}

// ResolvePath returns the config path for the given agent using the
// installer's home/cwd + goos overrides. CWDBased agents (Claude Code)
// resolve against the current working directory; all others resolve
// against the user's home directory.
func (i *Installer) ResolvePath(agent *Agent) (string, error) {
	if agent.PathFor == nil {
		return "", fmt.Errorf("agent %q has no PathFor resolver", agent.Name)
	}
	base, err := i.home()
	if agent.CWDBased {
		base, err = i.cwd()
	}
	if err != nil {
		return "", err
	}
	return agent.PathFor(base, i.goos())
}

// Install adds (or refreshes) the pad entry in the named agent's
// config. Returns the resolved path + whether the file was actually
// modified (false on a no-op refresh).
func (i *Installer) Install(agentName string) (string, bool, error) {
	if i.Binary == "" {
		return "", false, errors.New("Installer.Binary is required")
	}
	agent, err := FindAgent(agentName)
	if err != nil {
		return "", false, err
	}
	path, err := i.ResolvePath(agent)
	if err != nil {
		return "", false, err
	}
	modified, err := addEntry(agent, path, i.Binary)
	return path, modified, err
}

// Uninstall removes the pad entry from the named agent's config.
// Idempotent: a missing file or missing entry is not an error and
// returns (path, false, nil).
func (i *Installer) Uninstall(agentName string) (string, bool, error) {
	agent, err := FindAgent(agentName)
	if err != nil {
		return "", false, err
	}
	path, err := i.ResolvePath(agent)
	if err != nil {
		return "", false, err
	}
	removed, err := removeEntry(agent, path)
	return path, removed, err
}

// Status walks every global (per-user) agent and reports installation
// state. CWDBased agents (Claude Code) are omitted — their config is
// project-local, so a global status table can't meaningfully represent
// them; they're install-on-request only (see Agent.CWDBased). Per-agent
// failures are captured in AgentStatus.Error rather than
// short-circuiting the whole report.
func (i *Installer) Status() []AgentStatus {
	agents := globalAgents()
	out := make([]AgentStatus, 0, len(agents))
	for _, agent := range agents {
		row := AgentStatus{Name: agent.Name, Label: agent.Label}
		path, err := i.ResolvePath(agent)
		if err != nil {
			row.Error = err.Error()
			out = append(out, row)
			continue
		}
		row.ConfigPath = path
		installed, cmd, err := hasEntry(agent, path)
		if err != nil {
			row.Error = err.Error()
		}
		row.Installed = installed
		row.Command = cmd
		out = append(out, row)
	}
	return out
}
