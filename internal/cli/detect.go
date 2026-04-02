package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// ProjectInfo holds detected project metadata.
type ProjectInfo struct {
	Language   string   // go, node, rust, python, java, etc.
	BuildTool  string   // make, npm, cargo, pip, maven, etc.
	TestCmd    string   // detected test command
	HasCI      bool     // CI config detected
	CIProvider string   // github-actions, gitlab, circleci, etc.
	HasLinter  bool     // linter config detected
	Frameworks []string // detected frameworks (e.g., svelte, react, chi)
}

// DetectProject analyzes the current directory to determine project type,
// build system, test runner, CI, and other metadata.
func DetectProject(dir string) ProjectInfo {
	info := ProjectInfo{}

	// Detect language and build tool
	if fileExists(dir, "go.mod") {
		info.Language = "go"
		if fileExists(dir, "Makefile") {
			info.BuildTool = "make"
			info.TestCmd = detectMakeTestCmd(dir)
		} else {
			info.BuildTool = "go"
			info.TestCmd = "go test ./..."
		}
	} else if fileExists(dir, "package.json") {
		info.Language = "node"
		info.BuildTool = "npm"
		info.TestCmd = "npm test"
		if fileExists(dir, "yarn.lock") {
			info.BuildTool = "yarn"
			info.TestCmd = "yarn test"
		} else if fileExists(dir, "pnpm-lock.yaml") {
			info.BuildTool = "pnpm"
			info.TestCmd = "pnpm test"
		}
		// Check for TypeScript
		if fileExists(dir, "tsconfig.json") {
			info.Language = "typescript"
		}
	} else if fileExists(dir, "Cargo.toml") {
		info.Language = "rust"
		info.BuildTool = "cargo"
		info.TestCmd = "cargo test"
	} else if fileExists(dir, "pyproject.toml") || fileExists(dir, "setup.py") || fileExists(dir, "requirements.txt") {
		info.Language = "python"
		if fileExists(dir, "pyproject.toml") {
			info.BuildTool = "pip"
			info.TestCmd = "pytest"
		} else {
			info.BuildTool = "pip"
			info.TestCmd = "python -m pytest"
		}
	} else if fileExists(dir, "pom.xml") {
		info.Language = "java"
		info.BuildTool = "maven"
		info.TestCmd = "mvn test"
	} else if fileExists(dir, "build.gradle") || fileExists(dir, "build.gradle.kts") {
		info.Language = "java"
		info.BuildTool = "gradle"
		info.TestCmd = "gradle test"
	} else if fileExists(dir, "Makefile") {
		info.BuildTool = "make"
		info.TestCmd = detectMakeTestCmd(dir)
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
	} else if fileExists(dir, ".golangci.yml") || fileExists(dir, ".golangci.yaml") {
		info.HasLinter = true
	} else if fileExists(dir, ".prettierrc") || fileExists(dir, ".prettierrc.json") {
		info.HasLinter = true
	} else if fileExists(dir, "rustfmt.toml") || fileExists(dir, ".rustfmt.toml") {
		info.HasLinter = true
	} else if fileExists(dir, ".flake8") || fileExists(dir, "ruff.toml") || fileExists(dir, ".ruff.toml") {
		info.HasLinter = true
	}

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
