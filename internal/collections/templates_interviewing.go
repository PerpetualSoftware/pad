package collections

import "github.com/PerpetualSoftware/pad/internal/models"

// Trigger + scope vocabularies used by the interviewing (candidate-side)
// workspace's Conventions and Playbooks collections. Different enough
// from the hiring (company-side) template that both warrant their own
// vocabulary, even though they share the People category.
var (
	InterviewingConventionTriggers = []string{"always", "on-application-submitted", "on-interview-scheduled", "on-interview-completed", "on-stage-change", "on-offer-received", "on-rejection", "weekly-review"}
	InterviewingConventionScopes   = []string{"all", "research", "applications", "interviews", "followups"}
	InterviewingPlaybookTriggers   = []string{"on-application-submitted", "on-interview-scheduled", "on-interview-completed", "on-stage-change", "weekly-review", "manual"}
	InterviewingPlaybookScopes     = []string{"all", "research", "applications", "interviews", "followups"}
)

// interviewingTemplate builds the "interviewing" workspace template.
// Candidate-side job search: track Applications, Interviews per
// application, plus sibling collections for Companies you're researching
// and Contacts (referrals, recruiters, interviewers) referenced via
// wiki-links rather than parent/child.
func interviewingTemplate() WorkspaceTemplate {
	return WorkspaceTemplate{
		Name:        "interviewing",
		Category:    CategoryPeople,
		Description: "Applications, Interviews, Companies, Contacts, Docs",
		Icon:        "\U0001F4E8", // 📨
		Collections: []DefaultCollection{
			interviewingApplicationsCollection(0),
			interviewingInterviewsCollection(1),
			interviewingCompaniesCollection(2),
			interviewingContactsCollection(3),
			docsCollection(4),
			conventionsCollection(5, InterviewingConventionTriggers, InterviewingConventionScopes),
			playbooksCollection(6, InterviewingPlaybookTriggers, InterviewingPlaybookScopes),
		},
		Conventions: interviewingStarterConventions(),
		Playbooks:   interviewingStarterPlaybooks(),
		SeedItems:   interviewingSeedItems(),
	}
}

func interviewingApplicationsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Applications",
		Slug:        "applications",
		Prefix:      "APP",
		Icon:        "\U0001F4E8", // 📨
		Description: "Roles you're applying to or actively interviewing for",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "stage",
					Label:           "Stage",
					Type:            "select",
					Options:         []string{"researching", "applied", "screen", "interviewing", "offer", "accepted", "rejected", "withdrawn"},
					TerminalOptions: []string{"accepted", "rejected", "withdrawn"},
					Default:         "researching",
					Required:        true,
				},
				{
					Key:   "company",
					Label: "Company",
					Type:  "text",
				},
				{
					Key:   "role",
					Label: "Role",
					Type:  "text",
				},
				{
					Key:     "source",
					Label:   "Source",
					Type:    "select",
					Options: []string{"referral", "recruiter", "cold", "job-board", "network", "other"},
				},
				{
					Key:   "salary_range",
					Label: "Salary Range",
					Type:  "text",
				},
				{
					Key:     "remote",
					Label:   "Remote",
					Type:    "select",
					Options: []string{"yes", "hybrid", "no"},
				},
				{
					Key:   "applied_date",
					Label: "Applied Date",
					Type:  "date",
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

func interviewingInterviewsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Interviews",
		Slug:        "interviews",
		Prefix:      "INT",
		Icon:        "\U0001F5E3\uFE0F", // 🗣️
		Description: "Individual interview rounds, linked to an Application",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:     "round",
					Label:   "Round",
					Type:    "select",
					Options: []string{"screen", "technical", "behavioral", "onsite", "final"},
				},
				{
					Key:   "date",
					Label: "Date",
					Type:  "date",
				},
				{
					Key:   "interviewer",
					Label: "Interviewer",
					Type:  "text",
				},
				{
					Key:     "format",
					Label:   "Format",
					Type:    "select",
					Options: []string{"phone", "video", "onsite", "take-home"},
				},
				{
					Key:             "prep_status",
					Label:           "Prep Status",
					Type:            "select",
					Options:         []string{"not-started", "in-progress", "ready", "completed"},
					TerminalOptions: []string{"completed"},
					Default:         "not-started",
					Required:        true,
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "balanced",
			DefaultView:  "list",
			ListSortBy:   "date",
			BoardGroupBy: "prep_status",
		},
	}
}

func interviewingCompaniesCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Companies",
		Slug:        "companies",
		Prefix:      "CO",
		Icon:        "\U0001F3E2", // 🏢
		Description: "Companies you're researching or interviewing with",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "status",
					Label:           "Status",
					Type:            "select",
					Options:         []string{"interested", "researching", "active", "closed"},
					TerminalOptions: []string{"closed"},
					Default:         "interested",
				},
				{
					Key:   "industry",
					Label: "Industry",
					Type:  "text",
				},
				{
					Key:     "size",
					Label:   "Size",
					Type:    "select",
					Options: []string{"startup", "small", "medium", "large", "enterprise"},
				},
				{
					Key:   "glassdoor_rating",
					Label: "Glassdoor Rating",
					Type:  "number",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:      "balanced",
			DefaultView: "list",
			ListSortBy:  "updated_at",
		},
	}
}

func interviewingContactsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Contacts",
		Slug:        "contacts",
		Prefix:      "CON",
		Icon:        "\U0001F465", // 👥
		Description: "People you've interacted with — referrals, recruiters, interviewers",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:     "relationship",
					Label:   "Relationship",
					Type:    "select",
					Options: []string{"referral", "recruiter", "interviewer", "network", "other"},
				},
				{
					Key:   "company",
					Label: "Company",
					Type:  "text",
				},
				{
					Key:   "role",
					Label: "Role",
					Type:  "text",
				},
				{
					Key:   "last_contact",
					Label: "Last Contact",
					Type:  "date",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:      "balanced",
			DefaultView: "list",
			ListSortBy:  "last_contact",
		},
	}
}

// interviewingStarterConventions is the curated convention seed pack for
// candidate-side job-search workspaces.
func interviewingStarterConventions() []SeedConvention {
	return []SeedConvention{
		{
			Title:   "Create prep notes 48 hours before each interview",
			Content: "When an Interview is scheduled, create a Doc (or detailed content on the Interview item) with: the role and company context, expected questions for this round, STAR stories you'll use, and questions you'll ask them. Target 48 hours before the scheduled time so you have buffer to refine.",
			Fields:  `{"status":"active","trigger":"on-interview-scheduled","scope":"interviews","priority":"should"}`,
		},
		{
			Title:   "Log lessons when an Application ends (rejected or withdrawn)",
			Content: "When an Application hits a terminal stage (rejected, withdrawn), add a final comment summarizing what you learned — fit signals, question types that surprised you, rapport observations. Over the search this builds a personal retro that sharpens the next pass.",
			Fields:  `{"status":"active","trigger":"on-stage-change","scope":"applications","priority":"should"}`,
		},
		{
			Title:   "Send a thank-you within 24 hours of every interview",
			Content: "After each Interview Round completes, send a brief thank-you email to each interviewer within 24 hours. Reference something specific from the conversation. Log it on the Interview item (or a follow-up Task) so you can see at a glance who still needs one.",
			Fields:  `{"status":"active","trigger":"on-interview-completed","scope":"followups","priority":"should"}`,
		},
	}
}

// interviewingStarterPlaybooks is the curated playbook seed pack for the
// interviewing template.
func interviewingStarterPlaybooks() []SeedPlaybook {
	return []SeedPlaybook{
		{
			Title: "Log an Interview",
			Content: `1. Find the Interview item (or create it as a child of the Application if you haven't already).
2. Update prep_status to "completed".
3. Add a comment on the Interview with: how you felt it went, questions they asked, questions you asked, red/green flags, follow-up items.
4. Update the Application's stage if this interview advanced or ended the process.
5. Send a thank-you to each interviewer within 24h. Log it as a comment on the Interview item and update the matching Contact's last_contact field to today's date.
6. If you took away useful lessons (a question pattern, a company-specific insight), note it on the Company item so future applications there benefit.

💡 Ask your AI agent to customize this playbook for your job-search workflow.`,
			Fields: `{"status":"active","trigger":"on-interview-completed","scope":"interviews"}`,
		},
		{
			Title: "Weekly Job Search Review",
			Content: `1. List active Applications grouped by stage — where's the funnel thin? where's it thick?
2. Flag stalled Applications — anything without a stage-change or comment in the last 10 days. Decide: follow up, give up, or wait.
3. Review upcoming Interviews for the next 7 days — does each one have prep notes? Is any underprepared?
4. Review recent rejections and withdrawals — pull out lessons and capture them on the relevant Company or as Docs.
5. Look at your Contacts — is there anyone you owe a reply or update to? Any referral opportunities to pursue?
6. Update the pipeline: add new companies to research, close dead leads, prioritize top interests.
7. Set a couple of concrete goals for the next week (applications submitted, interviews scheduled, thank-yous sent).

💡 Ask your AI agent to customize this playbook for your job-search rhythm.`,
			Fields: `{"status":"active","trigger":"weekly-review","scope":"all"}`,
		},
		{
			Title: "Interviewing Workspace Onboarding",
			Content: `1. Ask the user what roles they're targeting — capture those as starter Applications (with stage="researching" if they haven't applied yet).
2. For each Application, capture the Company — create a Company item if it doesn't exist, or link by wiki-link from the Application content: [[Acme Corp]].
3. For active Applications with scheduled interviews, add Interview items with dates and prep_status.
4. Capture any Contacts the user wants to track — referrals, recruiters, networking connections.
5. Review the workspace's seeded conventions (prep lead time, retros on rejections, 24h thank-yous) and confirm they fit the user's style.
6. Suggest an initial Doc for their resume/cover-letter templates, or their STAR-story bank.
7. Suggest agent roles — Researcher, Applier, Interviewer prep — if roles don't already exist.

💡 Ask your AI agent to customize this onboarding for your own search workflow.`,
			Fields: `{"status":"active","trigger":"manual","scope":"all"}`,
		},
	}
}

// interviewingSeedItems seeds one example in each core collection.
func interviewingSeedItems() []SeedItem {
	return []SeedItem{
		{
			CollectionSlug: "applications",
			Title:          "Example Application: Senior Engineer at Acme Corp",
			Content:        "This is a seeded example. Delete or overwrite once your first real application lands.\n\nLink Company via wiki-link: [[Example Company: Acme Corp]]. Add Interview items as children when rounds are scheduled.",
			Fields:         `{"stage":"applied","company":"Acme Corp","role":"Senior Engineer","source":"referral","remote":"hybrid","applied_date":"2026-04-01"}`,
		},
		{
			CollectionSlug: "companies",
			Title:          "Example Company: Acme Corp",
			Content:        "This is a seeded example. Delete or overwrite once your first real company lands.\n\nUse this item to capture research notes about a company — culture, stack, team structure, interview process — that you want available regardless of how many roles you apply to there.",
			Fields:         `{"status":"interested","industry":"Software","size":"medium","glassdoor_rating":4.1}`,
		},
	}
}
