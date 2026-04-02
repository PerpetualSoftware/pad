package models

import "testing"

func TestParseWorkspaceSettingsEmpty(t *testing.T) {
	settings, err := ParseWorkspaceSettings("")
	if err != nil {
		t.Fatalf("ParseWorkspaceSettings error: %v", err)
	}
	if settings == nil {
		t.Fatal("expected settings struct")
	}
	if settings.Context != nil {
		t.Fatalf("expected nil context for empty settings, got %#v", settings.Context)
	}
}

func TestParseWorkspaceSettingsExtractsStructuredContext(t *testing.T) {
	settings, err := ParseWorkspaceSettings(`{"context":{"repositories":[{"name":"docapp","role":"primary","path":".","repo":"xarmian/pad"}],"paths":{"docs_repo":"../pad-web"},"commands":{"build":"make install","test":"go test ./..."},"stack":{"languages":["go","typescript"],"frameworks":["sveltekit"],"package_managers":["npm"]},"deployment":{"mode":"docker","base_url":"http://127.0.0.1:7777"},"assumptions":["pad-web lives at ../pad-web"]}}`)
	if err != nil {
		t.Fatalf("ParseWorkspaceSettings error: %v", err)
	}
	if settings.Context == nil {
		t.Fatal("expected context")
	}
	if len(settings.Context.Repositories) != 1 {
		t.Fatalf("expected 1 repository, got %#v", settings.Context.Repositories)
	}
	if settings.Context.Paths == nil || settings.Context.Paths.DocsRepo != "../pad-web" {
		t.Fatalf("expected docs repo path, got %#v", settings.Context.Paths)
	}
	if settings.Context.Commands == nil || settings.Context.Commands.Test != "go test ./..." {
		t.Fatalf("expected test command, got %#v", settings.Context.Commands)
	}
	if settings.Context.Stack == nil || len(settings.Context.Stack.Languages) != 2 {
		t.Fatalf("expected stack languages, got %#v", settings.Context.Stack)
	}
	if settings.Context.Deployment == nil || settings.Context.Deployment.Mode != "docker" {
		t.Fatalf("expected deployment mode docker, got %#v", settings.Context.Deployment)
	}
}

func TestSerializeWorkspaceSettingsRoundTrip(t *testing.T) {
	raw, err := SerializeWorkspaceSettings(&WorkspaceSettings{
		Context: &WorkspaceContext{
			Commands: &WorkspaceCommands{
				Build: "make install",
				Test:  "go test ./...",
			},
			Assumptions: []string{"Tasks should be PR-sized"},
		},
	})
	if err != nil {
		t.Fatalf("SerializeWorkspaceSettings error: %v", err)
	}

	context := ExtractWorkspaceContext(raw)
	if context == nil {
		t.Fatal("expected extracted context")
	}
	if context.Commands == nil || context.Commands.Build != "make install" {
		t.Fatalf("expected build command to survive round trip, got %#v", context.Commands)
	}
	if len(context.Assumptions) != 1 {
		t.Fatalf("expected assumptions to survive round trip, got %#v", context.Assumptions)
	}
}

func TestApplyWorkspaceContextPreservesUnknownSettings(t *testing.T) {
	raw, err := ApplyWorkspaceContext(`{"theme":"dark","context":{"assumptions":["old"]}}`, &WorkspaceContext{
		Assumptions: []string{"new"},
		Commands:    &WorkspaceCommands{Build: "make install"},
	})
	if err != nil {
		t.Fatalf("ApplyWorkspaceContext error: %v", err)
	}

	settingsMap, err := parseWorkspaceSettingsMap(raw)
	if err != nil {
		t.Fatalf("parseWorkspaceSettingsMap error: %v", err)
	}
	if settingsMap["theme"] != "dark" {
		t.Fatalf("expected unknown settings to be preserved, got %#v", settingsMap)
	}

	context := ExtractWorkspaceContext(raw)
	if context == nil || context.Commands == nil || context.Commands.Build != "make install" {
		t.Fatalf("expected context update to apply, got %#v", context)
	}
}

func TestNormalizeWorkspaceSettingsRejectsInvalidJSON(t *testing.T) {
	if _, err := NormalizeWorkspaceSettings(`{"context":`); err == nil {
		t.Fatal("expected invalid JSON to fail normalization")
	}
}

func TestExtractWorkspaceContextIgnoresInvalidSettings(t *testing.T) {
	if got := ExtractWorkspaceContext(`{"context":`); got != nil {
		t.Fatalf("expected invalid settings to return nil context, got %#v", got)
	}
}
