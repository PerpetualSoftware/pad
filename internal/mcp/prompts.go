package mcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Prompt names. Stable across versions — agents reference them by
// name in chat ("Use the pad_plan workflow"), so renaming breaks
// user-visible flows. Snake-case matches the tool-naming convention
// from TASK-945; we deliberately don't use "pad/plan" slash form
// because some MCP clients interpret slashes as namespace paths.
const (
	PromptPlan    = "pad_plan"
	PromptIdeate  = "pad_ideate"
	PromptRetro   = "pad_retro"
	PromptOnboard = "pad_onboard"
)

// padPrompts holds each prompt's display description + body. Body is
// emitted verbatim as a single user-role TextContent message — the
// canonical "system instructions" pattern for agent prompts.
//
// The bodies are lifted from skills/pad/SKILL.md (TASK-947). When
// SKILL.md changes, update the bodies here too — there's no
// auto-sync, but the lockstep is asserted by content tests below.
var padPrompts = map[string]struct {
	description string
	body        string
}{
	PromptPlan: {
		description: "Multi-step workflow to draft and decompose a Plan, lifted from /pad skill",
		body:        promptPlanBody,
	},
	PromptIdeate: {
		description: "Multi-step ideation workflow: explore an idea and capture as items, lifted from /pad skill",
		body:        promptIdeateBody,
	},
	PromptRetro: {
		description: "Retrospective workflow: review a completed Plan and capture lessons, lifted from /pad skill",
		body:        promptRetroBody,
	},
	PromptOnboard: {
		description: "Workspace onboarding workflow: scan, suggest conventions, propose initial plan, lifted from /pad skill",
		body:        promptOnboardBody,
	},
}

// RegisterPrompts installs the four static MCP prompts on srv. Each
// prompt takes no arguments — `prompts/get` returns a single
// user-role message containing the workflow text.
func RegisterPrompts(srv *server.MCPServer) {
	// Sorted iteration so prompts/list is deterministic for tests
	// and agents that cache by ordinal position (rare, but cheap).
	names := make([]string, 0, len(padPrompts))
	for n := range padPrompts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := padPrompts[name]
		prompt := mcp.NewPrompt(name, mcp.WithPromptDescription(spec.description))
		body := spec.body
		srv.AddPrompt(prompt, func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Description: spec.description,
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.NewTextContent(body),
					},
				},
			}, nil
		})
	}
}

// PromptBody returns the canonical body text for a registered prompt.
// Exported so tests can assert lockstep with SKILL.md without
// duplicating the literals. Unknown name → empty + error.
func PromptBody(name string) (string, error) {
	spec, ok := padPrompts[name]
	if !ok {
		return "", fmt.Errorf("unknown prompt %q", name)
	}
	return spec.body, nil
}
