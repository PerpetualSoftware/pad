package collections

// Onboarding seed items for new scrum-template workspaces.
//
// Same mechanism as the startup template's onboarding seeds (see
// templates_onboarding.go and DOC-1139): one seed item per user-facing
// collection, agent-invocable, written first-person from the workspace
// owner's future self. Any of BACK-1 / SPRINT-2 / BUG-3 / DOC-4 is a
// viable entry point for `/pad let's discuss <REF>`.
//
// Status of the post-signup hint that names the primary entry: as of
// this PR (TASK-1149) the CLI hint and dashboard banner are still
// hardcoded to IDEA-1 — they were shipped against the startup template
// only (PR #403). TASK-1150 makes those template-aware so a fresh scrum
// workspace surfaces "use pad to get BACK-1" rather than IDEA-1.
// Until TASK-1150 lands, the bodies are still reachable via direct
// fetch (`pad item show BACK-1`) but the dashboard banner doesn't
// promote them.
//
// Bodies and design rationale: DOC-1152 (PLAN-1146).
//
// Schema-valid terminal verbs per collection (locked at design time
// to prevent the round-1 regression on PR #402):
//   - Backlog → status `done`
//   - Sprints → status `completed`
//   - Bugs    → status `resolved` (or `wontfix`)
//   - Docs    → status `archived`

const scrumBacklogOnboardingBody = `# Welcome — let's start tracking what we're going to build

I just made this scrum workspace. It's where I'm going to keep track
of what the team is building: the backlog of work waiting to be picked
up, the sprints we're organizing it into, the bugs we're chasing
down, and the docs we want to keep.

If you're reading this, it's because I asked you to. I'd like your
help getting started — not with a tutorial, just by talking through
what's actually on our plate and capturing it here as we go.

## What I'd find useful

Ask me what the team is working on. Listen for the shape of it —
what's in flight, what's queued up, what's loose. As things come up,
capture them here. Backlog items for work-to-do, sprints when we
batch them up, bugs when we hit them, docs when we figure something
out worth remembering. Use my actual project, not toy data.

If something's small enough that we'll do it this sprint, capture it
as a backlog item with status ` + "`ready`" + `. If we're already in a sprint,
mark it ` + "`in_sprint`" + `. If it's broken code, that's a bug, not a backlog
item. Don't ask permission for every little thing — narrate briefly
what you're capturing as you go.

When we've got enough here to be useful, run ` + "`pad project dashboard`" + `
so I can see what we built, and point me at the web UI. Then mark
this backlog item done.

## A few things to know about Pad

- Items have IDs like BACK-3, SPRINT-7, BUG-12 — that's how I'll
  refer to them.
- ` + "`pad project next`" + ` will tell me what to work on.
- ` + "`pad project dashboard`" + ` shows the state of everything.
- ` + "`pad item search \"term\"`" + ` finds anything by content.
- ` + "`pad <anything>`" + ` in a /pad-aware agent session is how I'll talk
  to you in the future.

## If I've already done this before

If you look around and see I already have a backlog, sprints,
bugs of my own — I'm not new, I just re-opened you. Don't redo
any of this. Recap what's in the dashboard and let me get back
to work.
`

const scrumSprintOnboardingBody = `# A sprint I haven't planned yet

This is where my sprint cycles live — start dates, end dates, goals.
A sprint pulls a batch of backlog items (and sometimes bugs) into a
focused window.

## What I'd find useful

If you're with me through this item, I'm probably about to plan a
sprint. Help me think through it: what's the goal in plain language,
which backlog items are coming in, what's the start and end date,
what we're explicitly NOT doing this sprint. Pull from the existing
backlog rather than making up new work.

If the work I'm describing isn't sprint-shaped — it's a single quick
fix, or it's bigger than two weeks — capture it as a backlog item
or as a separate plan instead. Sprints earn their weight by holding
multiple items toward a shared outcome.

When the sprint's planned, run ` + "`pad project dashboard`" + ` so I can see
the shape of it. Then mark me completed when the sprint actually
ends — or delete me, I'm not precious.
`

const scrumBugOnboardingBody = `# A bug I haven't filed yet

Bugs live here, separate from feature work. They have their own
lifecycle — triaged, fixing, resolved, wontfix — because broken
things deserve a different conversation than new things.

## What I'd find useful

If you're with me through this item, something's probably broken or
behaving wrong. Help me capture it: what's broken, what should
happen, what does happen, how to reproduce, severity. Be specific —
"login fails" is a worse bug report than "OAuth callback drops
state= param when redirecting from /authorize, browser shows blank
page."

If the issue is small enough we'll just fix it now, that's fine —
file the bug anyway. Future-me will want the trail.

When it's fixed, mark me resolved (or wontfix if we decided not to
fix). Or delete me, I'm not precious.
`

const scrumDocOnboardingBody = `# A doc I haven't written yet

This is where I write things down that I'll need later —
architecture notes, decisions, retros, runbooks, the team norms
that aren't quite formal conventions yet.

## What I'd find useful

If you're with me through this item, I probably want to write
something down so I don't have to figure it out again next sprint.
Help me figure out what kind of doc this is — an architecture
sketch, a sprint retro, a runbook, a decision record — and what
should go in it. Capture as we go; the trick with docs is writing
them the moment something's fresh, not three sprints later.

If what I'm describing is more action than memory — "we should fix
this" or "let's add this feature" — capture it as a backlog item
or a bug instead. This is for memory.

When we've got the doc, show it back to me. Then archive me — or
delete me, I'm not precious.
`

// ScrumOnboardingItems returns the four onboarding seed items that ship
// in a fresh `scrum` workspace. The order matters: items are inserted
// in this slice's order before conventions/playbooks, so the
// workspace-scoped item_number sequence lands at BACK-1 / SPRINT-2 /
// BUG-3 / DOC-4. BACK-1 is the designated primary — TASK-1150 will
// wire the post-signup hint and dashboard banner to surface it.
//
// Backlog is the primary entry rather than Sprints because work
// originates in the backlog; sprints organize what's already there.
// "I want to start tracking what we're going to build" is itself a
// backlog item.
func ScrumOnboardingItems() []SeedItem {
	return []SeedItem{
		{
			CollectionSlug: "backlog",
			Title:          "Welcome — let's start tracking what we're going to build",
			Content:        scrumBacklogOnboardingBody,
			Fields:         `{"status":"new"}`,
		},
		{
			CollectionSlug: "sprints",
			Title:          "A sprint I haven't planned yet",
			Content:        scrumSprintOnboardingBody,
			Fields:         `{"status":"planning"}`,
		},
		{
			CollectionSlug: "bugs",
			Title:          "A bug I haven't filed yet",
			Content:        scrumBugOnboardingBody,
			Fields:         `{"status":"new"}`,
		},
		{
			CollectionSlug: "docs",
			Title:          "A doc I haven't written yet",
			Content:        scrumDocOnboardingBody,
			Fields:         `{"status":"draft"}`,
		},
	}
}
