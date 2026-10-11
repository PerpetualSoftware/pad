package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3555 phase 1: agent content writes become version-checked by default.
// In this phase nothing is refused. An unguarded write (no expected_seq, no
// expected_updated_at, no replace) is counted by door and kind, so phase 2's
// server-side refusal (428) is decided on measured traffic, and a content write
// from a door other than the web is told so in warnings.unguarded_content_write.

// Doors a write can arrive through, bounded for a metric label.
const (
	writeDoorWeb       = "web"        // a browser session (cookie)
	writeDoorCLI       = "cli"        // the pad CLI, including local stdio MCP, which runs it
	writeDoorAPI       = "api"        // a token-authenticated caller that is not the CLI
	writeDoorMCPRemote = "mcp_remote" // remote MCP over /mcp
	writeDoorChatGPT   = "chatgpt"    // the ChatGPT catalog
)

// Kinds of item update, by what the write can clobber.
const (
	writeKindContent = "content" // the body
	writeKindFields  = "fields"  // title, fields, tags, parent, assignment and other row members
)

// writeDoor names the door r arrived through. The ChatGPT surface is checked
// before remote MCP because it is one; the CLI before the generic token check
// because it authenticates with one.
func writeDoor(r *http.Request) string {
	if isChatGPTSurface(r) {
		return writeDoorChatGPT
	}
	if _, ok := remoteMCPCaller(r); ok {
		return writeDoorMCPRemote
	}
	if r.Header.Get("X-Pad-Client") == "cli" {
		return writeDoorCLI
	}
	if r.Header.Get("Authorization") != "" || isAPITokenAuth(r) {
		return writeDoorAPI
	}
	return writeDoorWeb
}

// updateWriteKind classifies an item update. An update that only appends
// (implementation notes, decisions, a comment) or carries nothing a later
// write could have changed returns "": the ruling exempts append-only
// operations, because they cannot clobber anything.
func updateWriteKind(in *models.ItemUpdate) string {
	if in.Content != nil {
		return writeKindContent
	}
	if in.Title != nil || in.Fields != nil || in.FieldsPatch != nil || in.Tags != nil ||
		in.Pinned != nil || in.SortOrder != nil || in.ParentID != nil ||
		in.AssignedUserID != nil || in.AgentRoleID != nil || in.ClearAssignedUser ||
		in.ClearAgentRole || in.GitHubPR != nil || in.ClearGitHubPR {
		return writeKindFields
	}
	return ""
}

// updateIsUnguarded reports whether an update carries neither a version token
// nor the deliberate-overwrite opt-out.
func updateIsUnguarded(in *models.ItemUpdate) bool {
	return in.ExpectedSeq == nil && in.ExpectedUpdatedAt == "" && !in.Replace
}
