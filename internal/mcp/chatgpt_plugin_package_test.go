package mcp

import (
	"encoding/json"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TASK-3321 U4: the ChatGPT plugin package in integrations/chatgpt is
// checked against the catalog it describes and against the submission
// limits OpenAI documents (developers.openai.com/plugins/deploy/submission).
// The skill and the test cases name tools by string, so a renamed or
// excluded tool turns this red instead of shipping a package that names a
// tool the server does not serve.

const chatGPTPluginDir = "../../integrations/chatgpt"

// chatGPTPluginMCPURL is where U2 mounts the catalog on Pad Cloud: the MCP
// host, beside the canonical /mcp resource (ruled on TASK-3321).
const chatGPTPluginMCPURL = "https://mcp.getpad.dev/mcp/chatgpt"

type pluginTestCase struct {
	Description      string `json:"description"`
	Prompt           string `json:"prompt"`
	ToolsTriggered   string `json:"tools_triggered"`
	ExpectedBehavior string `json:"expected_behavior"`
}

// chatGPTPluginPublisher is the legal entity, the only name the listing may
// use: there is no dba, so never "Perpetual Software" without "LLC".
const chatGPTPluginPublisher = "Perpetual Software LLC"

type pluginManifest struct {
	Name   string `json:"name"`
	Author struct {
		Name string `json:"name"`
	} `json:"author"`
	Extensions map[string]struct {
		Apps      any `json:"apps"`
		Hooks     any `json:"hooks"`
		Interface struct {
			DisplayName      string   `json:"displayName"`
			ShortDescription string   `json:"shortDescription"`
			LongDescription  string   `json:"longDescription"`
			DeveloperName    string   `json:"developerName"`
			Category         string   `json:"category"`
			Capabilities     []string `json:"capabilities"`
			WebsiteURL       string   `json:"websiteURL"`
			SupportURL       string   `json:"supportURL"`
			PrivacyPolicyURL string   `json:"privacyPolicyURL"`
			TermsURL         string   `json:"termsOfServiceURL"`
			DefaultPrompt    []string `json:"defaultPrompt"`
			BrandColor       string   `json:"brandColor"`
			BrandColorDark   string   `json:"brandColorDark"`
			ComposerIcon     string   `json:"composerIcon"`
			Logo             string   `json:"logo"`
		} `json:"interface"`
		Review struct {
			TestCases struct {
				Positive []pluginTestCase `json:"positive"`
				Negative []pluginTestCase `json:"negative"`
			} `json:"test_cases"`
		} `json:"review"`
	} `json:"extensions"`
}

func readPluginFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chatGPTPluginDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func chatGPTToolNames() map[string]bool {
	names := map[string]bool{}
	for _, tool := range ChatGPTCatalog {
		names[tool.Name] = true
	}
	return names
}

func TestChatGPTPlugin_Manifest(t *testing.T) {
	var m pluginManifest
	if err := json.Unmarshal(readPluginFile(t, "plugin.json"), &m); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(m.Name) || len(m.Name) > 64 {
		t.Errorf("name %q: want lowercase letters, numbers and single hyphens, at most 64", m.Name)
	}
	if m.Author.Name != chatGPTPluginPublisher {
		t.Errorf("author.name = %q, want %q", m.Author.Name, chatGPTPluginPublisher)
	}
	oa, ok := m.Extensions["com.openai"]
	if !ok {
		t.Fatal(`plugin.json has no extensions["com.openai"]`)
	}
	// A ZIP carrying app references or lifecycle hooks cannot be submitted.
	if oa.Apps != nil || oa.Hooks != nil {
		t.Error("plugin.json declares apps or hooks, which submission refuses")
	}
	in := oa.Interface
	limits := []struct {
		field, value string
		max          int
	}{
		{"displayName", in.DisplayName, 30},
		{"shortDescription", in.ShortDescription, 30},
		{"longDescription", in.LongDescription, 4000},
		{"developerName", in.DeveloperName, 80},
		{"category", in.Category, 1 << 10},
		{"websiteURL", in.WebsiteURL, 1024},
		{"supportURL", in.SupportURL, 1024},
		{"privacyPolicyURL", in.PrivacyPolicyURL, 1024},
		{"termsOfServiceURL", in.TermsURL, 1024},
	}
	for _, l := range limits {
		if n := len([]rune(l.value)); n == 0 || n > l.max {
			t.Errorf("%s is %d characters, want 1..%d", l.field, n, l.max)
		}
	}
	for _, u := range []string{in.WebsiteURL, in.SupportURL, in.PrivacyPolicyURL, in.TermsURL} {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("%q: listing URLs must be https", u)
		}
	}
	if len(in.DefaultPrompt) > 3 {
		t.Errorf("%d starter prompts, at most 3", len(in.DefaultPrompt))
	}
	for _, p := range in.DefaultPrompt {
		if len([]rune(p)) > 128 || strings.Contains(p, "@") {
			t.Errorf("starter prompt %q: at most 128 characters and no @mentions", p)
		}
	}
	if len(in.Capabilities) > 20 {
		t.Errorf("%d capabilities, at most 20", len(in.Capabilities))
	}
	for _, c := range in.Capabilities {
		if len([]rune(c)) > 120 {
			t.Errorf("capability %q over 120 characters", c)
		}
	}
	if in.DeveloperName != chatGPTPluginPublisher {
		t.Errorf("developerName = %q, want %q", in.DeveloperName, chatGPTPluginPublisher)
	}
	checkContrast(t, "brandColor", in.BrandColor, "#FFFFFF")
	checkContrast(t, "brandColorDark", in.BrandColorDark, "#212121")
	for field, p := range map[string]string{"logo": in.Logo, "composerIcon": in.ComposerIcon} {
		checkIcon(t, field, p)
	}
}

func TestChatGPTPlugin_TestCases(t *testing.T) {
	var m pluginManifest
	if err := json.Unmarshal(readPluginFile(t, "plugin.json"), &m); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	tc := m.Extensions["com.openai"].Review.TestCases
	// Initial MCP review requires exactly five positive and three negative.
	if len(tc.Positive) != 5 || len(tc.Negative) != 3 {
		t.Fatalf("test cases: %d positive and %d negative, want exactly 5 and 3", len(tc.Positive), len(tc.Negative))
	}
	names := chatGPTToolNames()
	for i, c := range tc.Positive {
		if c.Description == "" || c.Prompt == "" || c.ToolsTriggered == "" || c.ExpectedBehavior == "" {
			t.Errorf("positive case %d: description, prompt, tools_triggered and expected_behavior are all required", i)
		}
		if len([]rune(c.Description)) > 4000 {
			t.Errorf("positive case %d: description over 4000 characters", i)
		}
		for _, name := range strings.Split(c.ToolsTriggered, ",") {
			if name = strings.TrimSpace(name); !names[name] {
				t.Errorf("positive case %d triggers %q, which is not a ChatGPT catalog tool", i, name)
			}
		}
	}
	for i, c := range tc.Negative {
		if c.Description == "" || c.Prompt == "" {
			t.Errorf("negative case %d: description and prompt are required", i)
		}
	}
}

func TestChatGPTPlugin_MCPConfig(t *testing.T) {
	var cfg struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(readPluginFile(t, "mcp.json"), &cfg); err != nil {
		t.Fatalf("mcp.json: %v", err)
	}
	// Only one MCP server can be connected per plugin.
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("mcp.json declares %d servers, want 1", len(cfg.MCPServers))
	}
	for name, s := range cfg.MCPServers {
		if s.Type != "streamable-http" || s.URL != chatGPTPluginMCPURL {
			t.Errorf("server %q = {%s %s}, want {streamable-http %s}", name, s.Type, s.URL, chatGPTPluginMCPURL)
		}
	}
}

// chatGPTSkillForbidden are the marks of a skill written for a shell: CLI
// verbs, flags, local paths and config files. ChatGPT has none of these.
var chatGPTSkillForbidden = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bpad (item|project|collection|workspace|bootstrap|playbook|auth|server|init|attachment|role|library|mcp|agent|session|github|webhook|db)\b`),
	regexp.MustCompile(`(^|\s)--[a-z]`),
	regexp.MustCompile("```"),
	regexp.MustCompile(`~/|\.pad\.toml|\.pad/`),
	regexp.MustCompile(`(?i)\b(CLI|command line|terminal|shell|PATH)\b`),
	regexp.MustCompile(`\bpad_[a-z_]+\b`),
}

func TestChatGPTPlugin_SkillIsMCPOnly(t *testing.T) {
	skill := string(readPluginFile(t, "skills/pad/SKILL.md"))
	front := regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n`).FindStringSubmatch(skill)
	if front == nil || !strings.Contains(front[1], "name: pad") || !strings.Contains(front[1], "description: ") {
		t.Fatal("SKILL.md needs name and description frontmatter")
	}
	for _, re := range chatGPTSkillForbidden {
		if loc := re.FindStringIndex(skill); loc != nil {
			t.Errorf("SKILL.md contains %q (matched %s); the skill must name only catalog tools", skill[loc[0]:loc[1]], re)
		}
	}
	// Every backticked snake_case identifier is a catalog tool or one of
	// its parameters.
	allowed := chatGPTToolNames()
	for _, tool := range ChatGPTCatalog {
		for _, p := range tool.Params {
			allowed[p] = true
		}
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z]+(?:_[a-z]+)+|[a-z]+)`").FindAllStringSubmatch(skill, -1) {
		if !allowed[m[1]] {
			t.Errorf("SKILL.md names `%s`, which is neither a ChatGPT catalog tool nor one of its parameters", m[1])
		}
		used[m[1]] = true
	}
	// The connect flow the catalog's instructions describe.
	for _, name := range []string{"list_workspaces", "get_workspace_overview", "archive_item", "restore_item"} {
		if !used[name] {
			t.Errorf("SKILL.md never names %s", name)
		}
	}
}

func checkIcon(t *testing.T, field, p string) {
	t.Helper()
	if !strings.HasPrefix(p, "./") {
		t.Errorf("%s %q must be a ./-relative path", field, p)
		return
	}
	f, err := os.Open(filepath.Join(chatGPTPluginDir, p))
	if err != nil {
		t.Errorf("%s: %v", field, err)
		return
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Errorf("%s %s: not a PNG: %v", field, p, err)
		return
	}
	if cfg.Width != cfg.Height || cfg.Width < 48 || cfg.Width > 4096 {
		t.Errorf("%s %s is %dx%d, want square, 48..4096", field, p, cfg.Width, cfg.Height)
	}
}

func relativeLuminance(hex string) (float64, bool) {
	if !regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`).MatchString(hex) {
		return 0, false
	}
	var ch [3]float64
	for i := range ch {
		var v int
		for _, c := range strings.ToLower(hex[1+2*i : 3+2*i]) {
			v = v*16 + strings.IndexRune("0123456789abcdef", c)
		}
		x := float64(v) / 255
		if x <= 0.03928 {
			ch[i] = x / 12.92
		} else {
			ch[i] = math.Pow((x+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2], true
}

func checkContrast(t *testing.T, field, color, against string) {
	t.Helper()
	a, ok := relativeLuminance(color)
	if !ok {
		t.Errorf("%s %q: want #RRGGBB", field, color)
		return
	}
	b, _ := relativeLuminance(against)
	hi, lo := math.Max(a, b), math.Min(a, b)
	if r := (hi + 0.05) / (lo + 0.05); r < 2 {
		t.Errorf("%s %s has %.2f:1 contrast against %s, want at least 2:1", field, color, r, against)
	}
}
