package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Installation records where a skill was installed for a specific tool in a specific project.
type Installation struct {
	// ProjectPath is the absolute path to the project directory.
	ProjectPath string `json:"project_path"`
	// Workspace is the workspace slug (from .pad.toml), if known.
	Workspace string `json:"workspace,omitempty"`
	// Tool is the canonical tool name (e.g., "claude", "agents", "copilot").
	Tool string `json:"tool"`
	// SkillPath is the full path to the installed skill file.
	SkillPath string `json:"skill_path"`
	// InstalledAt is when the skill was last installed or updated.
	InstalledAt time.Time `json:"installed_at"`
	// Version is the pad binary version that wrote this installation.
	Version string `json:"version,omitempty"`
}

// Registry tracks all skill installations across projects for a user.
type Registry struct {
	Installations []Installation `json:"installations"`
}

// registryPath returns ~/.pad/installations.json.
func registryPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pad", "installations.json"), nil
}

// LoadRegistry reads the installation registry from disk.
// Returns an empty registry if the file doesn't exist.
func LoadRegistry() (*Registry, error) {
	path, err := registryPath()
	if err != nil {
		return &Registry{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}

	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		// Corrupted file — start fresh
		return &Registry{}, nil
	}
	return &reg, nil
}

// Save writes the registry to disk.
func (r *Registry) Save() error {
	path, err := registryPath()
	if err != nil {
		return err
	}

	// Ensure ~/.pad/ exists
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}

	return os.WriteFile(path, data, 0600)
}

// Record adds or updates an installation entry.
func (r *Registry) Record(projectPath, workspace, tool, skillPath, version string) {
	now := time.Now().UTC()

	// Update existing entry if same project + tool
	for i := range r.Installations {
		inst := &r.Installations[i]
		if inst.ProjectPath == projectPath && inst.Tool == tool {
			inst.SkillPath = skillPath
			inst.Workspace = workspace
			inst.InstalledAt = now
			inst.Version = version
			return
		}
	}

	// Add new entry
	r.Installations = append(r.Installations, Installation{
		ProjectPath: projectPath,
		Workspace:   workspace,
		Tool:        tool,
		SkillPath:   skillPath,
		InstalledAt: now,
		Version:     version,
	})
}

// Prune removes entries whose skill files no longer exist on disk.
func (r *Registry) Prune() int {
	pruned := 0
	kept := r.Installations[:0]
	for _, inst := range r.Installations {
		if _, err := os.Stat(inst.SkillPath); err == nil {
			kept = append(kept, inst)
		} else {
			pruned++
		}
	}
	r.Installations = kept
	return pruned
}

// InstallationStatus describes the state of a tracked installation.
type InstallationStatus struct {
	Installation
	Exists   bool `json:"exists"`
	Outdated bool `json:"outdated"`
	// Edited and Newer are the files an update keeps (BUG-3466): not what pad
	// wrote, or written by a newer pad. Neither counts as Outdated.
	Edited bool `json:"edited,omitempty"`
	Newer  bool `json:"newer,omitempty"`
}

// Status checks each tracked installation and returns its current state,
// decided the way an update would decide it (DecideSkillWrite). embedded is
// the raw embedded skill; version is this pad's.
func (r *Registry) Status(embedded []byte, version string) []InstallationStatus {
	var results []InstallationStatus
	for _, inst := range r.Installations {
		s := InstallationStatus{Installation: inst}

		data, err := os.ReadFile(inst.SkillPath)
		if err != nil {
			s.Exists = false
			s.Outdated = true
			results = append(results, s)
			continue
		}
		s.Exists = true

		tool := ResolveTool(inst.Tool)
		if tool == nil {
			// Unknown tool: compare raw.
			s.Outdated = !bytes.Equal(normalizeSkill(data), normalizeSkill(embedded))
			results = append(results, s)
			continue
		}
		switch DecideSkillWrite(*tool, data, true, FormatForTool(*tool, embedded), version).Action {
		case SkillUpdate:
			s.Outdated = true
		case SkillKeepEdited:
			s.Edited = true
		case SkillKeepNewer:
			s.Newer = true
		}
		results = append(results, s)
	}
	return results
}

// UpdateAll updates all tracked installations that are outdated. A file that
// was edited, or that a newer pad wrote, is kept unless force (BUG-3466) and
// described in kept, naming the force command. Returns the number of
// installations updated, the ones kept, and any errors encountered.
func (r *Registry) UpdateAll(embeddedContent []byte, version string, force bool) (updated int, kept []string, errors []error) {
	for i := range r.Installations {
		inst := &r.Installations[i]

		tool := ResolveTool(inst.Tool)
		if tool == nil {
			errors = append(errors, fmt.Errorf("%s: unknown tool %q", inst.ProjectPath, inst.Tool))
			continue
		}

		// Check if file exists
		currentData, err := os.ReadFile(inst.SkillPath)
		if err != nil {
			errors = append(errors, fmt.Errorf("%s (%s): file missing, skipping", inst.ProjectPath, tool.Label))
			continue
		}

		expected := FormatForTool(*tool, embeddedContent)
		d := DecideSkillWrite(*tool, currentData, true, expected, version)
		switch d.Action {
		case SkillUnchanged:
			continue // already up to date
		case SkillKeepEdited:
			if !force {
				kept = append(kept, fmt.Sprintf("%s (%s): kept, it was edited; replace it with pad agent update --force", inst.ProjectPath, tool.Label))
				continue
			}
		case SkillKeepNewer:
			if !force {
				kept = append(kept, fmt.Sprintf("%s (%s): kept, pad %s wrote it and this is pad %s; replace it with pad agent update --force", inst.ProjectPath, tool.Label, d.StampVersion, version))
				continue
			}
		}
		expected = StampSkill(expected, version)

		// Ensure directory exists (in case it was partially deleted)
		if err := os.MkdirAll(filepath.Dir(inst.SkillPath), 0755); err != nil {
			errors = append(errors, fmt.Errorf("%s (%s): %w", inst.ProjectPath, tool.Label, err))
			continue
		}

		if err := os.WriteFile(inst.SkillPath, expected, 0644); err != nil {
			errors = append(errors, fmt.Errorf("%s (%s): %w", inst.ProjectPath, tool.Label, err))
			continue
		}

		inst.InstalledAt = time.Now().UTC()
		inst.Version = version
		updated++
	}

	return updated, kept, errors
}
