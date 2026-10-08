package mcp

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// BUG-3463: the Claude Code plugin must not pin a version. A `version` in
// plugin.json (or in its marketplace entry) pins every installed copy until
// the string changes, however many commits touch plugin/; with none, a
// relative-path plugin in a git-hosted marketplace is versioned by its
// commit, so `/plugin update` picks up every change. 0.3.3 stood from
// 2026-08-27 while 8 commits changed plugin/ under it.
// https://code.claude.com/docs/en/plugins/loading#versions-and-updates

const (
	claudePluginManifest = "../../plugin/.claude-plugin/plugin.json"
	claudeMarketplace    = "../../.claude-plugin/marketplace.json"
)

func pinnedVersion(t *testing.T, path string, entries func(map[string]any) []map[string]any) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var pinned []string
	for _, e := range entries(doc) {
		if v, ok := e["version"]; ok {
			pinned = append(pinned, path+": "+jsonString(v))
		}
	}
	return pinned
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func manifestEntries(doc map[string]any) []map[string]any { return []map[string]any{doc} }

func marketplaceEntries(doc map[string]any) []map[string]any {
	var out []map[string]any
	plugins, _ := doc["plugins"].([]any)
	for _, p := range plugins {
		if m, ok := p.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestClaudePlugin_NoPinnedVersion(t *testing.T) {
	pinned := pinnedVersion(t, claudePluginManifest, manifestEntries)
	pinned = append(pinned, pinnedVersion(t, claudeMarketplace, marketplaceEntries)...)
	for _, p := range pinned {
		t.Errorf("plugin version pinned at %s: installed copies stop updating until it changes; remove it (BUG-3463)", p)
	}
	// Control: the marketplace walk must reach the pad entry, or an
	// entry gaining a version would pass unseen.
	b, _ := os.ReadFile(claudeMarketplace)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	if n := len(marketplaceEntries(doc)); n == 0 {
		t.Fatal("marketplace.json lists no plugins; the guard is reading the wrong file")
	}
}

// TASK-3487: the marketplace's pad entry follows STABLE releases. Its source is
// either the in-repo "./plugin" (before the first stable tag that carries an
// unpinned plugin) or a git-subdir of this repository's plugin/ pinned to a
// vX.Y.Z tag, which scripts/pin-plugin-marketplace.sh writes after a stable
// release. Never an rc tag, a branch, or a sha the release did not pick.
func TestClaudePlugin_MarketplaceFollowsStableReleases(t *testing.T) {
	b, err := os.ReadFile(claudeMarketplace)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range marketplaceEntries(doc) {
		if e["name"] != "pad" {
			continue
		}
		found = true
		switch src := e["source"].(type) {
		case string:
			if src != "./plugin" {
				t.Errorf("pad source %q: want \"./plugin\" or a git-subdir pinned to a stable tag", src)
			}
		case map[string]any:
			ref, _ := src["ref"].(string)
			if src["source"] != "git-subdir" || src["url"] != "https://github.com/PerpetualSoftware/pad.git" || src["path"] != "plugin" ||
				!stableTag.MatchString(ref) || len(src) != 4 {
				t.Errorf("pad source %v: want exactly {git-subdir, https://github.com/PerpetualSoftware/pad.git, plugin, vX.Y.Z}; the https URL, since the owner/repo shorthand clones over SSH", src)
			}
		default:
			t.Errorf("pad source %v has an unexpected shape", src)
		}
	}
	if !found {
		t.Fatal("marketplace.json has no pad entry")
	}
}

var stableTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
