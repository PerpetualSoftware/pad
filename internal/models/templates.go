package models

import "time"

type Template struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Content     string `json:"content,omitempty"`
}

var Templates = []Template{
	{
		Type:        "roadmap",
		Name:        "Roadmap",
		Description: "High-level project scope, goals, and phased plan",
		Icon:        "\U0001F4CB",
		Content: `# {Title}

## Vision

What are we building and why?

## Goals

- [ ] Goal 1
- [ ] Goal 2
- [ ] Goal 3

## Phases

### Phase 1: {Name}

**Status:** Not Started
**Target:** {Timeframe}

Summary of what this phase covers.

### Phase 2: {Name}

**Status:** Not Started
**Target:** {Timeframe}

Summary of what this phase covers.

### Phase 3: {Name}

**Status:** Not Started
**Target:** {Timeframe}

Summary of what this phase covers.

## Success Criteria

How do we know when we're done?

- [ ] Criterion 1
- [ ] Criterion 2
- [ ] Criterion 3

## Open Questions

- Question 1
- Question 2
`,
	},
	{
		Type:        "plan",
		Name:        "Plan",
		Description: "Scoped implementation plan for a specific milestone",
		Icon:        "\U0001F3D7\uFE0F",
		Content: `# {Title}

## Overview

What does this plan accomplish? What's the scope?

## Prerequisites

What needs to be done before this plan can start?

- [ ] Prerequisite 1
- [ ] Prerequisite 2

## Tasks

- [ ] Task 1
- [ ] Task 2
- [ ] Task 3
- [ ] Task 4
- [ ] Task 5

## Technical Details

Implementation specifics, patterns, libraries, key decisions.

## Edge Cases & Risks

- **Risk:** Description and mitigation
- **Edge case:** Description and handling

## Definition of Done

- [ ] Criteria 1
- [ ] Criteria 2
- [ ] Criteria 3

## Notes

Additional context, links, or observations.
`,
	},
	{
		Type:        "architecture",
		Name:        "Architecture",
		Description: "System design, data models, and technical decisions",
		Icon:        "\U0001F9E0",
		Content: `# {Title}

## Overview

High-level description of this architectural component or decision.

## Context

Why does this exist? What problem does it solve? What constraints are we working within?

## Design

### Data Model

Describe the core data structures.

### API / Interface

How do other components interact with this?

### Flow

Step-by-step flow for the primary use case.

## Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| | | |

## Trade-offs

What did we choose NOT to do, and why?

## Dependencies

What does this depend on? What depends on this?

## Future Considerations

What might change? What are we deferring?
`,
	},
	{
		Type:        "ideation",
		Name:        "Ideation",
		Description: "Brainstorms, rough ideas, and explorations",
		Icon:        "\U0001F4A1",
		Content: `# {Title}

## The Idea

What's the core concept? Describe it simply.

## Problem

What problem does this solve? Who has this problem?

## How It Might Work

Initial thoughts on implementation or approach.

## Questions to Explore

- Question 1
- Question 2
- Question 3

## Pros

- Pro 1
- Pro 2

## Cons / Risks

- Con 1
- Con 2

## Related

Links to related documents, references, or prior art.

## Next Steps

What would need to happen to move this forward?
`,
	},
	{
		Type:        "feature-spec",
		Name:        "Feature Spec",
		Description: "Detailed specification for a specific feature",
		Icon:        "\U0001F4C4",
		Content: `# {Title}

## Summary

One paragraph describing the feature.

## Motivation

Why build this? What user need does it address?

## User Stories

- As a [user], I want to [action] so that [benefit]
- As a [user], I want to [action] so that [benefit]

## Detailed Design

### Behavior

How does the feature work from the user's perspective?

### UI / UX

Describe the interface.

### API Changes

New or modified API endpoints.

### Data Changes

New or modified data structures.

## Edge Cases

- Edge case 1: How it's handled
- Edge case 2: How it's handled

## Out of Scope

What is explicitly NOT part of this feature?

## Test Plan

- [ ] Test scenario 1
- [ ] Test scenario 2
- [ ] Test scenario 3

## Open Questions

- Question 1
- Question 2
`,
	},
	{
		Type:        "notes",
		Name:        "Notes",
		Description: "General thoughts, meeting notes, or observations",
		Icon:        "\U0001F4DD",
		Content: `# {Title}

## Notes

Start writing here.
`,
	},
	{
		Type:        "prompt-library",
		Name:        "Prompt Library",
		Description: "Reusable prompts and agent instructions",
		Icon:        "\U0001F4AC",
		Content: `# {Title}

## Overview

What are these prompts for? When should they be used?

## Prompts

### {Prompt Name}

**Use when:** Description of when to use this prompt.

` + "```" + `
Prompt text goes here.
` + "```" + `

### {Prompt Name}

**Use when:** Description of when to use this prompt.

` + "```" + `
Prompt text goes here.
` + "```" + `

## Guidelines

Notes on how to use these prompts effectively.
`,
	},
	{
		Type:        "reference",
		Name:        "Reference",
		Description: "External information, API docs, or specs for context",
		Icon:        "\U0001F4DA",
		Content: `# {Title}

## Source

Where does this information come from? Include links if applicable.

## Content

Reference content goes here.

## Notes

Additional observations or context about this reference material.
`,
	},
}

func GetTemplate(docType string) *Template {
	for _, t := range Templates {
		if t.Type == docType {
			return &t
		}
	}
	return nil
}

// CustomTemplate is a user-created template stored in the database.
type CustomTemplate struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	DocType     string    `json:"doc_type"`
	Icon        string    `json:"icon"`
	Content     string    `json:"content,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CustomTemplateCreate struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	DocType     string `json:"doc_type"`
	Icon        string `json:"icon"`
	Content     string `json:"content"`
}
