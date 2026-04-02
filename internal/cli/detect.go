package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectInfo holds detected project metadata.
type ProjectInfo struct {
	Language        string   // go, node, rust, python, java, etc.
	BuildTool       string   // make, npm, cargo, pip, maven, etc.
	BuildCmd        string   // detected build command
	SetupCmd        string   // detected setup/install command
	TestCmd         string   // detected test command
	LintCmd         string   // detected lint/format command
	DevCmd          string   // detected dev/run command
	WebBuildCmd     string   // detected frontend build command
	PackageManagers []string // detected package managers (e.g., npm, go)
	HasCI           bool     // CI config detected
	CIProvider      string   // github-actions, gitlab, circleci, etc.
	HasLinter       bool     // linter config detected
	Frameworks      []string // detected frameworks (e.g., svelte, react, chi)
}

// DetectProject analyzes the current directory to determine project type,
// build system, test runner, CI, and other metadata.
func DetectProject(dir string) ProjectInfo {
	info := ProjectInfo{}

	// Detect language and build tool
	if fileExists(dir, "go.mod") {
		info.Language = "go"
		info.PackageManagers = append(info.PackageManagers, "go")
		if fileExists(dir, "Makefile") {
			info.BuildTool = "make"
			info.BuildCmd = detectMakeBuildCmd(dir)
			info.SetupCmd = detectMakeSetupCmd(dir)
			info.TestCmd = detectMakeTestCmd(dir)
			info.DevCmd = detectMakeDevCmd(dir)
		} else {
			info.BuildTool = "go"
			info.BuildCmd = "go build ./..."
			info.TestCmd = "go test ./..."
		}
	} else if fileExists(dir, "package.json") {
		info.Language = "node"
		info.BuildTool = "npm"
		info.BuildCmd = "npm run build"
		info.SetupCmd = "npm install"
		info.TestCmd = "npm test"
		info.PackageManagers = append(info.PackageManagers, "npm")
		if fileExists(dir, "yarn.lock") {
			info.BuildTool = "yarn"
			info.BuildCmd = "yarn build"
			info.SetupCmd = "yarn install"
			info.TestCmd = "yarn test"
			info.PackageManagers = []string{"yarn"}
		} else if fileExists(dir, "pnpm-lock.yaml") {
			info.BuildTool = "pnpm"
			info.BuildCmd = "pnpm build"
			info.SetupCmd = "pnpm install"
			info.TestCmd = "pnpm test"
			info.PackageManagers = []string{"pnpm"}
		}
		// Check for TypeScript
		if fileExists(dir, "tsconfig.json") {
			info.Language = "typescript"
		}
	} else if fileExists(dir, "Cargo.toml") {
		info.Language = "rust"
		info.BuildTool = "cargo"
		info.BuildCmd = "cargo build"
		info.SetupCmd = "cargo fetch"
		info.TestCmd = "cargo test"
		info.PackageManagers = append(info.PackageManagers, "cargo")
	} else if fileExists(dir, "pyproject.toml") || fileExists(dir, "setup.py") || fileExists(dir, "requirements.txt") {
		info.Language = "python"
		if fileExists(dir, "pyproject.toml") {
			info.BuildTool = "pip"
			info.SetupCmd = "pip install -e ."
			info.TestCmd = "pytest"
		} else {
			info.BuildTool = "pip"
			info.SetupCmd = "pip install -r requirements.txt"
			info.TestCmd = "python -m pytest"
		}
		info.PackageManagers = append(info.PackageManagers, "pip")
	} else if fileExists(dir, "pom.xml") {
		info.Language = "java"
		info.BuildTool = "maven"
		info.BuildCmd = "mvn compile"
		info.SetupCmd = "mvn dependency:resolve"
		info.TestCmd = "mvn test"
		info.PackageManagers = append(info.PackageManagers, "maven")
	} else if fileExists(dir, "build.gradle") || fileExists(dir, "build.gradle.kts") {
		info.Language = "java"
		info.BuildTool = "gradle"
		info.BuildCmd = "gradle build"
		info.SetupCmd = "gradle dependencies"
		info.TestCmd = "gradle test"
		info.PackageManagers = append(info.PackageManagers, "gradle")
	} else if fileExists(dir, "Makefile") {
		info.BuildTool = "make"
		info.BuildCmd = detectMakeBuildCmd(dir)
		info.SetupCmd = detectMakeSetupCmd(dir)
		info.TestCmd = detectMakeTestCmd(dir)
		info.DevCmd = detectMakeDevCmd(dir)
	}

	// Detect CI
	if dirExists(dir, ".github", "workflows") {
		info.HasCI = true
		info.CIProvider = "github-actions"
	} else if fileExists(dir, ".gitlab-ci.yml") {
		info.HasCI = true
		info.CIProvider = "gitlab"
	} else if dirExists(dir, ".circleci") {
		info.HasCI = true
		info.CIProvider = "circleci"
	}

	// Detect linter
	if fileExists(dir, ".eslintrc.json") || fileExists(dir, ".eslintrc.js") || fileExists(dir, "eslint.config.js") || fileExists(dir, "eslint.config.mjs") {
		info.HasLinter = true
		info.LintCmd = "npm run lint"
	} else if fileExists(dir, ".golangci.yml") || fileExists(dir, ".golangci.yaml") {
		info.HasLinter = true
		info.LintCmd = "golangci-lint run"
	} else if fileExists(dir, ".prettierrc") || fileExists(dir, ".prettierrc.json") {
		info.HasLinter = true
		info.LintCmd = "prettier --check ."
	} else if fileExists(dir, "rustfmt.toml") || fileExists(dir, ".rustfmt.toml") {
		info.HasLinter = true
		info.LintCmd = "cargo fmt --check"
	} else if fileExists(dir, ".flake8") || fileExists(dir, "ruff.toml") || fileExists(dir, ".ruff.toml") {
		info.HasLinter = true
		info.LintCmd = "ruff check ."
	}

	detectFrontendProject(dir, &info)
	info.PackageManagers = uniqueNonEmpty(info.PackageManagers)
	info.Frameworks = uniqueNonEmpty(info.Frameworks)

	return info
}

// SuggestedConventions returns convention titles from the library that match
// the detected project info. Returns a map of title → customized content.
func SuggestedConventions(info ProjectInfo) map[string]string {
	suggestions := make(map[string]string)

	// Build convention
	if info.BuildTool != "" {
		var buildCmd string
		switch info.BuildTool {
		case "make":
			buildCmd = "make build"
		case "npm":
			buildCmd = "npm run build"
		case "yarn":
			buildCmd = "yarn build"
		case "pnpm":
			buildCmd = "pnpm build"
		case "cargo":
			buildCmd = "cargo build"
		case "go":
			buildCmd = "go build ./..."
		case "maven":
			buildCmd = "mvn compile"
		case "gradle":
			buildCmd = "gradle build"
		default:
			buildCmd = info.BuildTool + " build"
		}
		suggestions["Rebuild after code changes"] = "After modifying source code, run `" + buildCmd + "` to verify everything compiles and builds successfully."
	}

	// Test convention
	if info.TestCmd != "" {
		suggestions["Run tests before completing tasks"] = "Run the project's test suite (`" + info.TestCmd + "`) before marking any task as done. If tests fail, fix them before completing the task."
	}

	// Linter convention
	if info.HasLinter {
		suggestions["Run linter before committing"] = "Run the project's linter/formatter before committing code to ensure consistent code style."
	}

	// CI convention
	if info.HasCI {
		suggestions["Verify locally before PR"] = "Before creating a PR, verify the changes work locally: build succeeds, tests pass, and the feature works as expected."
	}

	// Always suggest these general ones
	suggestions["Commit after task completion"] = "Create a git commit with a descriptive message after completing each discrete unit of work. Reference the task slug or item number in the commit message."
	suggestions["Update task status when starting work"] = "When starting work on a task, update its status to in-progress: `pad item update <ref> --status in-progress`"

	return suggestions
}

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func dirExists(parts ...string) bool {
	info, err := os.Stat(filepath.Join(parts...))
	return err == nil && info.IsDir()
}

func detectMakeTestCmd(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return "make test"
	}
	content := string(data)
	// Check if Makefile has a test target
	if strings.Contains(content, "test:") || strings.Contains(content, "test :") {
		return "make test"
	}
	// Fall back to language-specific if no test target
	if strings.Contains(content, "go test") {
		return "go test ./..."
	}
	return "make test"
}

func detectMakeBuildCmd(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return "make build"
	}
	content := string(data)
	if strings.Contains(content, "build:") || strings.Contains(content, "build :") {
		return "make build"
	}
	if strings.Contains(content, "go build") {
		return "go build ./..."
	}
	return "make build"
}

func detectMakeSetupCmd(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return ""
	}
	content := string(data)
	if strings.Contains(content, "install:") || strings.Contains(content, "install :") {
		return "make install"
	}
	return ""
}

func detectMakeDevCmd(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return ""
	}
	content := string(data)
	if strings.Contains(content, "dev:") || strings.Contains(content, "dev :") {
		return "make dev"
	}
	if strings.Contains(content, "serve:") || strings.Contains(content, "serve :") {
		return "make serve"
	}
	return ""
}

type packageManifest struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Scripts         map[string]string `json:"scripts"`
}

func detectFrontendProject(dir string, info *ProjectInfo) {
	webDir := filepath.Join(dir, "web")
	if !fileExists(webDir, "package.json") {
		return
	}

	info.PackageManagers = append(info.PackageManagers, detectNodePackageManager(webDir))

	manifest, err := readPackageManifest(filepath.Join(webDir, "package.json"))
	if err != nil {
		return
	}

	deps := map[string]string{}
	for name, version := range manifest.Dependencies {
		deps[name] = version
	}
	for name, version := range manifest.DevDependencies {
		deps[name] = version
	}

	if _, ok := deps["@sveltejs/kit"]; ok {
		info.Frameworks = append(info.Frameworks, "sveltekit")
		info.PackageManagers = append(info.PackageManagers, "npm")
		if info.Language == "" {
			info.Language = "typescript"
		}
	}
	if _, ok := deps["react"]; ok {
		info.Frameworks = append(info.Frameworks, "react")
	}
	if _, ok := deps["vue"]; ok {
		info.Frameworks = append(info.Frameworks, "vue")
	}

	pm := detectNodePackageManager(webDir)
	if scriptExists(manifest.Scripts, "build") {
		info.WebBuildCmd = nodeScriptCmd(pm, "build")
	}
	if scriptExists(manifest.Scripts, "lint") && info.LintCmd == "" {
		info.LintCmd = nodeScriptCmd(pm, "lint")
		info.HasLinter = true
	}
	if scriptExists(manifest.Scripts, "dev") && info.DevCmd == "" {
		info.DevCmd = "cd web && " + nodeScriptCmd(pm, "dev")
	}
}

func detectNodePackageManager(dir string) string {
	if fileExists(dir, "yarn.lock") {
		return "yarn"
	}
	if fileExists(dir, "pnpm-lock.yaml") {
		return "pnpm"
	}
	return "npm"
}

func nodeScriptCmd(pm, script string) string {
	switch pm {
	case "yarn":
		return "yarn " + script
	case "pnpm":
		return "pnpm " + script
	default:
		return "npm run " + script
	}
}

func scriptExists(scripts map[string]string, key string) bool {
	if scripts == nil {
		return false
	}
	_, ok := scripts[key]
	return ok
}

func readPackageManifest(path string) (*packageManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
