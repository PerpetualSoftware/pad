package cli

import "testing"

func TestTASK4412_IsCommitmentRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/workspaces/ws/items/TASK-5/commitment":           true,
		"/api/v1/workspaces/ws/items/TASK-5/commitment/links":     true,
		"/api/v1/workspaces/ws/items/TASK-5/commitment/move?x=1":  true,
		"/api/v1/workspaces/ws/commitment-links/abc":              true,
		"/api/v1/workspaces/ws/commitment-gates/abc":              true,
		"/workspaces/ws/items/TASK-5/commitment":                  true,
		"/pad/api/v1/workspaces/ws/items/TASK-5/commitment/links": true,
		"/a/b/api/v1/workspaces/ws/commitment-gates/abc":          true,
		"/api/v1/workspaces/ws/items/TASK-5/move":                 false,
		"/api/v1/workspaces/ws/items/TASK-5":                      false,
		"/api/v1/workspaces/ws/commitment-links":                  false,
		"/api/v1/workspaces/ws/reminders":                         false,
		"/pad/api/v1/workspaces/ws/items/TASK-5/copy":             false,
		"/api/v1/workspaces/api/items/v1/commitment":              true,
		"/pad/api/v1/workspaces/api/items/v1/commitment":          true,
		"": false,
	} {
		if got := IsCommitmentRoute(path); got != want {
			t.Errorf("IsCommitmentRoute(%q) = %v, want %v", path, got, want)
		}
	}
}
