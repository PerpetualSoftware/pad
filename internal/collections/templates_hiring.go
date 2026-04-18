package collections

import "github.com/xarmian/pad/internal/models"

// Trigger + scope vocabularies used by the hiring workspace's Conventions
// and Playbooks collections. Separate from the software vocabularies so a
// hiring workspace can have rules tied to domain-specific moments
// (on-candidate-advance, on-offer-extended) without bleeding into the
// software templates' set.
var (
	HiringConventionTriggers = []string{"always", "on-candidate-advance", "on-loop-scheduled", "on-feedback-submitted", "on-offer-extended", "on-close-requisition"}
	HiringConventionScopes   = []string{"all", "sourcing", "screening", "interviewing", "offers"}
	HiringPlaybookTriggers   = []string{"on-candidate-advance", "on-interview-scheduled", "on-feedback-submitted", "on-close-requisition", "manual"}
	HiringPlaybookScopes     = []string{"all", "sourcing", "screening", "interviewing", "offers"}
)

// hiringTemplate builds the "hiring" workspace template. Company-side
// hiring: track Requisitions → Candidates → Interview Loops → Feedback.
// Parent/child handles the chain — no new reference mechanism needed.
func hiringTemplate() WorkspaceTemplate {
	return WorkspaceTemplate{
		Name:        "hiring",
		Category:    CategoryPeople,
		Description: "Requisitions, Candidates, Interview Loops, Feedback, Docs",
		Icon:        "\U0001F465", // 👥
		Collections: []DefaultCollection{
			hiringRequisitionsCollection(0),
			hiringCandidatesCollection(1),
			hiringLoopsCollection(2),
			hiringFeedbackCollection(3),
			docsCollection(4),
			conventionsCollection(5, HiringConventionTriggers, HiringConventionScopes),
			playbooksCollection(6, HiringPlaybookTriggers, HiringPlaybookScopes),
		},
		Conventions: hiringStarterConventions(),
		Playbooks:   hiringStarterPlaybooks(),
		SeedItems:   hiringSeedItems(),
	}
}

func hiringRequisitionsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Requisitions",
		Slug:        "requisitions",
		Prefix:      "REQ",
		Icon:        "\U0001F4CB", // 📋
		Description: "Open roles you're hiring for",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "status",
					Label:           "Status",
					Type:            "select",
					Options:         []string{"open", "on-hold", "filled", "closed"},
					TerminalOptions: []string{"filled", "closed"},
					Default:         "open",
					Required:        true,
				},
				{
					Key:   "team",
					Label: "Team",
					Type:  "text",
				},
				{
					Key:     "level",
					Label:   "Level",
					Type:    "select",
					Options: []string{"intern", "junior", "mid", "senior", "staff", "principal"},
				},
				{
					Key:   "location",
					Label: "Location",
					Type:  "text",
				},
				{
					Key:   "target_start",
					Label: "Target Start",
					Type:  "date",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "balanced",
			DefaultView:  "board",
			BoardGroupBy: "status",
			ListSortBy:   "target_start",
		},
	}
}

func hiringCandidatesCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Candidates",
		Slug:        "candidates",
		Prefix:      "CAND",
		Icon:        "\U0001F464", // 👤
		Description: "Applicants moving through the hiring pipeline",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "stage",
					Label:           "Stage",
					Type:            "select",
					Options:         []string{"sourced", "applied", "screen", "onsite", "offer", "hired", "rejected", "withdrawn"},
					TerminalOptions: []string{"hired", "rejected", "withdrawn"},
					Default:         "applied",
					Required:        true,
				},
				{
					Key:     "source",
					Label:   "Source",
					Type:    "select",
					Options: []string{"inbound", "referral", "agency", "outbound", "event", "other"},
				},
				{
					Key:   "recruiter",
					Label: "Recruiter",
					Type:  "text",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "balanced",
			DefaultView:  "board",
			BoardGroupBy: "stage",
			ListSortBy:   "updated_at",
		},
	}
}

func hiringLoopsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Interview Loops",
		Slug:        "interview-loops",
		Prefix:      "LOOP",
		Icon:        "\U0001F501", // 🔁
		Description: "Interview rounds scheduled for candidates",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:     "loop_type",
					Label:   "Loop Type",
					Type:    "select",
					Options: []string{"screen", "technical", "onsite", "final"},
				},
				{
					Key:   "date",
					Label: "Date",
					Type:  "date",
				},
				{
					Key:             "result",
					Label:           "Result",
					Type:            "select",
					Options:         []string{"pending", "advance", "hold", "reject"},
					TerminalOptions: []string{"advance", "hold", "reject"},
					Default:         "pending",
					Required:        true,
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "balanced",
			DefaultView:  "list",
			ListSortBy:   "date",
			BoardGroupBy: "result",
		},
	}
}

func hiringFeedbackCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Feedback",
		Slug:        "feedback",
		Prefix:      "FB",
		Icon:        "\U0001F4AC", // 💬
		Description: "Individual interviewer feedback for each loop",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:   "interviewer",
					Label: "Interviewer",
					Type:  "text",
				},
				{
					Key:     "recommendation",
					Label:   "Recommendation",
					Type:    "select",
					Options: []string{"strong-hire", "hire", "mixed", "no-hire", "strong-no"},
				},
				{
					Key:             "submitted",
					Label:           "Submitted",
					Type:            "select",
					Options:         []string{"pending", "submitted"},
					TerminalOptions: []string{"submitted"},
					Default:         "pending",
					Required:        true,
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "content-primary",
			DefaultView:  "list",
			ListSortBy:   "updated_at",
			BoardGroupBy: "recommendation",
		},
	}
}

// hiringStarterConventions is the curated convention seed pack for hiring
// workspaces. Kept small and universal; workspace owners can add more from
// the library or compose their own.
func hiringStarterConventions() []SeedConvention {
	return []SeedConvention{
		{
			Title:   "Never paste candidate PII into comments or content",
			Content: "Keep emails, phone numbers, addresses, and any identifying PII out of free-text comments and item content. Use the structured fields (candidate title, recruiter field) so access control and redaction work. If you need to record a note tied to a candidate's real identity, put it in the structured field, not prose.",
			Fields:  `{"status":"active","trigger":"always","scope":"all","priority":"must"}`,
		},
		{
			Title:   "Every Candidate should link to a Requisition",
			Content: "Create Candidates as children of the Requisition they're being considered for. This makes the open-roles view a complete picture of who is in-flight per req and simplifies closing the req when it's filled.",
			Fields:  `{"status":"active","trigger":"always","scope":"sourcing","priority":"should"}`,
		},
		{
			Title:   "Record debrief outcome within 24h of an interview loop",
			Content: "After an Interview Loop completes, submit Feedback items for every scheduled interviewer and set the loop's result (advance/hold/reject) within 24 hours. Stale loops without a decision blur the pipeline and leave candidates hanging.",
			Fields:  `{"status":"active","trigger":"on-feedback-submitted","scope":"interviewing","priority":"should"}`,
		},
	}
}

// hiringStarterPlaybooks is the curated playbook seed pack for hiring
// workspaces.
func hiringStarterPlaybooks() []SeedPlaybook {
	return []SeedPlaybook{
		{
			Title: "Advance a Candidate",
			Content: `1. Update the Candidate's stage field to the new stage (sourced → applied → screen → onsite → offer → hired).
2. If the new stage introduces an interview round, create an Interview Loop as a child of the Candidate with the appropriate loop_type and a scheduled date.
3. For each interviewer on the loop, create a Feedback item as a child of the Loop with recommendation=pending.
4. Update the Candidate's content with a short note about the advance and why (fit notes, recruiter observations).
5. Notify the Recruiter field's named person (outside the workspace — email, slack, etc.) that the stage changed.

💡 Ask your AI agent to customize this playbook for your team's specific interview process and tooling.`,
			Fields: `{"status":"active","trigger":"on-candidate-advance","scope":"all"}`,
		},
		{
			Title: "Hiring Workspace Onboarding",
			Content: `1. Ask the user what role they're hiring for first — capture it as a Requisition (team, level, location, target start date).
2. Prompt them to add any Candidates already in-flight as children of the Requisition, with the right stage.
3. For each Candidate actively interviewing, prompt to add their scheduled Interview Loops and any submitted Feedback.
4. Review the workspace's seeded conventions — PII handling, requisition linking, 24h debriefs — and confirm the user is comfortable with them.
5. Suggest creating agent roles for the workspace: Recruiter, Hiring Manager, Interviewer. Don't auto-create; ask.
6. Draft a Doc capturing the team's interview rubric (or offer to import from an existing rubric if they have one).

💡 Ask your AI agent to customize this playbook for your team's specific interview process and tooling.`,
			Fields: `{"status":"active","trigger":"manual","scope":"all"}`,
		},
	}
}

// hiringSeedItems seeds a single example item in each core collection so
// users can see the shape of the workspace without having to build it from
// scratch. Users can delete or edit these.
func hiringSeedItems() []SeedItem {
	return []SeedItem{
		{
			CollectionSlug: "requisitions",
			Title:          "Example Requisition: Senior Backend Engineer",
			Content:        "This is a seeded example. Delete or overwrite once your first real requisition lands.\n\n## What we're hiring for\n\nSenior backend engineer on the Platform team. 5+ years building distributed systems. Remote-first, target start in the next quarter.",
			Fields:         `{"status":"open","team":"Platform","level":"senior","location":"Remote","target_start":"2026-07-01"}`,
		},
		{
			CollectionSlug: "candidates",
			Title:          "Example Candidate: Alex Rivera",
			Content:        "This is a seeded example. Delete or overwrite once your first real candidate lands.\n\nLink to the open role via parent-child: `pad item update <this-slug> --parent REQ-1` (or create with `--parent REQ-1`).",
			Fields:         `{"stage":"screen","source":"referral","recruiter":"Jamie Chen"}`,
		},
	}
}
