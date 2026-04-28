package cli

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BuildWorkspaceContext converts detected project metadata plus the current Pad
// client configuration into a machine-readable workspace context.
func BuildWorkspaceContext(dir string, info ProjectInfo, cfg *config.Config) *models.WorkspaceContext {
	context := &models.WorkspaceContext{
		Repositories: detectWorkspaceRepositories(dir),
		Paths:        detectWorkspacePaths(dir),
		Commands:     detectWorkspaceCommands(info),
		Stack: &models.WorkspaceStack{
			Languages:       uniqueNonEmpty([]string{info.Language}),
			Frameworks:      uniqueNonEmpty(info.Frameworks),
			PackageManagers: uniqueNonEmpty(info.PackageManagers),
		},
		Deployment:  detectWorkspaceDeployment(cfg),
		Assumptions: detectWorkspaceAssumptions(dir, cfg),
	}

	if len(context.Repositories) == 0 {
		context.Repositories = nil
	}
	if context.Paths != nil && *context.Paths == (models.WorkspacePaths{}) {
		context.Paths = nil
	}
	if context.Commands != nil && *context.Commands == (models.WorkspaceCommands{}) {
		context.Commands = nil
	}
	if context.Stack != nil && len(context.Stack.Languages) == 0 && len(context.Stack.Frameworks) == 0 && len(context.Stack.PackageManagers) == 0 {
		context.Stack = nil
	}
	if context.Deployment != nil && *context.Deployment == (models.WorkspaceDeployment{}) {
		context.Deployment = nil
	}
	if len(context.Assumptions) == 0 {
		context.Assumptions = nil
	}

	return context
}

func detectWorkspaceRepositories(dir string) []models.WorkspaceRepository {
	var repos []models.WorkspaceRepository
	repos = append(repos, models.WorkspaceRepository{
		Name: filepath.Base(dir),
		Role: "primary",
		Path: ".",
		Repo: detectGitRemoteSlug(dir),
	})

	docsPath := filepath.Join(dir, "..", "pad-web")
	if fileExists(docsPath, "package.json") {
		repos = append(repos, models.WorkspaceRepository{
			Name: filepath.Base(docsPath),
			Role: "docs",
			Path: "../pad-web",
			Repo: detectGitRemoteSlug(docsPath),
		})
	}

	return repos
}

func detectWorkspacePaths(dir string) *models.WorkspacePaths {
	paths := &models.WorkspacePaths{
		Root: ".",
	}
	if fileExists(filepath.Join(dir, "..", "pad-web"), "package.json") {
		paths.DocsRepo = "../pad-web"
	}
	if fileExists(dir, "package.json") {
		paths.Web = "."
	} else if dirExists(dir, "web") {
		paths.Web = "web"
	}
	if dirExists(dir, "internal", "server") {
		paths.Server = "internal/server"
	}
	if dirExists(dir, "skills") {
		paths.Skills = "skills"
	}
	if fileExists(dir, ".pad.toml") {
		paths.Config = ".pad.toml"
	}
	return paths
}

func detectWorkspaceCommands(info ProjectInfo) *models.WorkspaceCommands {
	return &models.WorkspaceCommands{
		Setup:  info.SetupCmd,
		Build:  info.BuildCmd,
		Test:   info.TestCmd,
		Lint:   info.LintCmd,
		Dev:    info.DevCmd,
		Start:  info.DevCmd,
		Web:    info.WebBuildCmd,
		Format: "",
	}
}

func detectWorkspaceDeployment(cfg *config.Config) *models.WorkspaceDeployment {
	if cfg == nil {
		return nil
	}

	deployment := &models.WorkspaceDeployment{
		Mode:    cfg.Mode,
		BaseURL: cfg.BaseURL(),
	}
	if cfg.Mode == config.ModeLocal {
		deployment.Host = cfg.Host
	}
	return deployment
}

func detectWorkspaceAssumptions(dir string, cfg *config.Config) []string {
	var assumptions []string
	if fileExists(filepath.Join(dir, "..", "pad-web"), "package.json") {
		assumptions = append(assumptions, "A companion docs or marketing repo lives at ../pad-web")
	}
	if cfg != nil {
		switch cfg.Mode {
		case config.ModeCloud:
			assumptions = append(assumptions, "This client connects to Pad Cloud at "+config.CloudBaseURL)
		case config.ModeRemote:
			assumptions = append(assumptions, "This client connects to a self-hosted remote Pad server")
		case config.ModeLocal:
			assumptions = append(assumptions, "This client manages a local Pad server by default")
		}
	}
	if dirExists(dir, "web") && dirExists(dir, "internal", "server") {
		assumptions = append(assumptions, "This repository contains both server and web application surfaces")
	}
	return uniqueNonEmpty(assumptions)
}

func detectGitRemoteSlug(dir string) string {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return normalizeGitRemoteSlug(out.String())
}

func normalizeGitRemoteSlug(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, ".git")
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		return strings.TrimPrefix(raw, "git@github.com:")
	case strings.HasPrefix(raw, "https://github.com/"):
		return strings.TrimPrefix(raw, "https://github.com/")
	default:
		return ""
	}
}
