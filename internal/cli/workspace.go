package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// PadToml represents the per-project workspace link file.
type PadToml struct {
	Workspace string `toml:"workspace"`
	// URL is the base URL of the Pad server hosting this workspace (e.g.
	// "https://app.getpad.dev" or a self-hosted remote). When set, it
	// overrides the user's global ~/.pad/config.toml URL so the directory
	// targets the right server regardless of which workspace the user's
	// default config points at. Empty for local-mode workspaces (the
	// default loopback server is implied). See BUG-1535.
	URL       string `toml:"url,omitempty"`
	AgentName string `toml:"agent_name,omitempty"` // optional: identifies which AI agent is acting
	// Push carries per-repository push/consent settings (PLAN-2613 S2).
	// A pointer so an absent `[push]` table is distinguishable from one
	// written with every field at its zero value — though for AutoArm the
	// two mean the same thing (not opted in), the distinction keeps the
	// door open for future push settings where it would matter, and lets
	// PadTomlAutoArm answer without a second nil check leaking out.
	Push *PadTomlPush `toml:"push,omitempty"`
}

// PadTomlPush is the `[push]` table in a repository's .pad.toml
// (PLAN-2613 S2, D4).
type PadTomlPush struct {
	// AutoArm, when true, opts THIS repository's sessions into arming at
	// connect — declaring consent to receive `pad push` notifications
	// without an explicit in-session `pad session arm` (D4). It is the
	// ONLY per-repo enabler, and it lives in a committed file on purpose:
	// D4's "explicit file edit = deliberate act" is the whole consent
	// story for the auto path. Default (absent/false) is not opted in —
	// see ResolveAutoArm for how a per-user setting can veto a true here
	// but nothing can turn arming on machine-wide.
	AutoArm bool `toml:"auto_arm"`
}

// PadTomlAutoArm reports whether this .pad.toml opts the repository into
// auto-arm. Nil-safe: a nil *PadToml (no workspace linked) or a missing
// `[push]` table both read as "not opted in", so callers need no guard.
func (p *PadToml) PadTomlAutoArm() bool {
	return p != nil && p.Push != nil && p.Push.AutoArm
}

// DetectWorkspace walks up the directory tree from cwd looking for .pad.toml.
func DetectWorkspace(flagOverride string) (string, error) {
	if flagOverride != "" {
		return flagOverride, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := cwd
	for {
		configPath := filepath.Join(dir, ".pad.toml")
		if _, err := os.Stat(configPath); err == nil {
			var cfg PadToml
			if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
				return "", fmt.Errorf("parse %s: %w", configPath, err)
			}
			if cfg.Workspace != "" {
				return cfg.Workspace, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("no workspace linked. Run 'pad workspace init' to create one")
}

// LoadPadToml finds and reads the nearest .pad.toml by walking up from cwd.
// Returns nil if no .pad.toml is found (not an error).
func LoadPadToml() (*PadToml, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	dir := cwd
	for {
		configPath := filepath.Join(dir, ".pad.toml")
		if _, err := os.Stat(configPath); err == nil {
			var cfg PadToml
			if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", configPath, err)
			}
			return &cfg, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return nil, nil
}

// WriteWorkspaceLink writes a .pad.toml in the given directory.
//
// serverURL is the base URL of the Pad server hosting this workspace. Pass
// the empty string for local-mode workspaces; pass cfg.BaseURL() (or
// equivalently cfg.URL) for any non-local mode (remote, cloud) so that the
// directory targets the right server even when the user's global config
// points at a different one. See BUG-1535.
func WriteWorkspaceLink(dir, slug, serverURL string) error {
	path := filepath.Join(dir, ".pad.toml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(PadToml{Workspace: slug, URL: serverURL})
}
