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
	AgentName string `toml:"agent_name,omitempty"` // optional: identifies which AI agent is acting
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

	return "", fmt.Errorf("no workspace linked. Run 'pad init' to create one")
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
func WriteWorkspaceLink(dir, slug string) error {
	path := filepath.Join(dir, ".pad.toml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(PadToml{Workspace: slug})
}
