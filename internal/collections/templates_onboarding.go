package collections

// Onboarding seed items for new software-category workspaces.
//
// Every fresh `startup` workspace ships with one seed item per user-facing
// collection (Ideas, Plans, Tasks, Docs). Each is a first-person note from
// the workspace owner's future self that an agent can fetch and meaningfully
// converse around. The post-signup hint points users at IDEA-1 first because
// "I want to start using Pad" is itself an idea, but any of the four is a
// viable entry point for `/pad let's discuss <REF>`.
//
// Design philosophy (see PLAN-1131, DOC-1139):
//
//   - No marker, no skill detection, no schema fields. Bodies are plain
//     markdown. The seeder picks them based on the template — same way it
//     already picks conventions and playbooks.
//
//   - Bodies are inserted *before* conventions and playbooks (the existing
//     seeder loop in store/collections.go runs SeedItems first), so the
//     workspace-scoped item_number sequence lands at IDEA-1 / PLAN-2 /
//     TASK-3 / DOC-4 — discoverable, predictable refs the post-signup hint
//     can name without computation.
//
//   - The four bodies cross-reference each other in plain language ("if it's
//     bigger, make a plan; if it's earlier, catch it as an idea") so a user
//     fetching any one of them learns the conceptual shape of the others.
//     Doc takes a different angle (knowledge capture, not action).
//
// Scope: software defaults only — startup template (Tasks, Ideas, Plans,
// Docs collections). Scrum and product templates have different collection
// sets (Backlog/Sprints/Bugs and Features/Feedback/Roadmap respectively)
// and need their own bodies; tracked as follow-up under PLAN-1131.
// People-category templates (hiring, interviewing) are tracked under
// PLAN-1140.

// ideaOnboardingBody is the body of IDEA-1 — the primary agent-invocable
// onboarding entry point. The post-signup hint names this ref specifically.
const ideaOnboardingBody = `# Welcome — let's get this place set up

I just made this workspace. It's where I'm going to keep track of what
I'm working on: the plans I have going, the tasks I need to do, ideas
I want to come back to, the occasional doc.

If you're reading this, it's because I asked you to. I'd like your help
getting started — not with a tutorial, just by talking through what I'm
working on right now and capturing it here as we go.

## What I'd find useful

Ask me what I'm working on. Listen for the shape of it — the project,
where I am with it, what's in front of me this week. As things come up
in what I tell you, capture them here using ` + "`pad`" + `. A plan, a few tasks
under it, maybe an idea or two I want to remember. Use my actual project,
not toy data. Don't ask permission for every little thing — just narrate
briefly what you're capturing as you go, the way a good notetaker would.

If at any point I want you to stop or skip ahead, do that. I'm not
looking for thoroughness — I'm looking for momentum.

When it feels like I've got enough here to be useful, run
` + "`pad project dashboard`" + ` so I can see what we built, and point me at
the web UI so I can poke around there too. Then mark this idea done.

## A few things to know about Pad

- Items have IDs like TASK-3, IDEA-7, PLAN-2 — that's how I'll refer to them.
- ` + "`pad project next`" + ` will tell me what to work on.
- ` + "`pad project dashboard`" + ` shows the state of everything.
- ` + "`pad item search \"term\"`" + ` finds anything by content.
- ` + "`pad <anything>`" + ` in a /pad-aware agent session is how I'll talk to you
  in the future — same as I just did to bring you here.

## If I've already done this before

If you look around the workspace and see I already have plans, tasks,
ideas of my own — I'm not new, I just re-opened you. Don't redo any
of this. Recap what's in the dashboard, remind me what I can do, and
let me get back to work.

That's it. Thanks for the help.
`

// planOnboardingBody is the body of PLAN-2. Angle: "something brewing
// that's bigger than one task."
const planOnboardingBody = `# A plan I haven't written yet

This is where my bigger pieces of work live — anything that takes more
than a single change or has multiple steps. A plan has a goal at the top,
tasks under it, and a status. It's where I think out loud about a chunk
of work before splitting it into tasks.

## What I'd find useful

If you're with me through this item, I've probably got something brewing
— a feature, a refactor, a milestone, a launch — that's too big for a
single task. Help me think through it: what's the goal in plain language,
what's in scope, what's out, what's the rough sequence, what has to happen
first. As we work it out, capture it as a real plan and split off tasks
under it. Use my actual project, not toy data.

If what I'm describing turns out to be small enough to just be one task,
capture it that way and skip the plan. If it's earlier than that — more
"I wonder if we should do X" than "we're doing X" — capture it as an
idea so I can come back to it. The collection isn't precious; capturing
the thought is.

When we've got something useful, run ` + "`pad project dashboard`" + ` so I can
see it. Then mark me done — or delete me, I'm not precious either.
`

// taskOnboardingBody is the body of TASK-3. Angle: "something concrete
// on my plate, low ceremony."
const taskOnboardingBody = `# A task I haven't named yet

These are the bite-sized things — PR-sized, ideally. A single change, a
single decision, a single thing I can finish in one go. Tasks can live
under a plan or stand alone. When I run ` + "`pad project next`" + ` I usually get
pointed at one of these.

## What I'd find useful

If you're with me through this item, I've probably got something specific
in mind — a bug to fix, a small feature, a chore — and I want to capture
it without too much ceremony. Help me name it, set a priority, and figure
out whether it belongs under a plan or stands alone. If a couple of
related tasks come up while we talk, that might be the start of a plan;
we can spin one up and link them.

If it's bigger than one task can hold, we'll make a plan instead. If
it's earlier than action — still a question, not a commitment — we'll
catch it as an idea.

When we're done, run ` + "`pad project next`" + ` so I can see what to work on
first. Then mark me done — or delete me, I'm not precious.
`

// docOnboardingBody is the body of DOC-4. Angle: "knowledge capture,
// not action."
const docOnboardingBody = `# A doc I haven't written yet

This is where I write things down that I'll need later — architecture
notes, decisions, runbooks, retros, the conventions I haven't quite
formalized yet. Docs are searchable like everything else, and they don't
have a status in the same way tasks do; they exist until they're stale,
then I update or archive them.

## What I'd find useful

If you're with me through this item, I probably want to write something
down so I don't have to figure it out again next month. Help me figure
out what kind of doc this is — an architecture sketch, a decision record,
a how-to, a retro — and what should go in it. Ask the questions a future
me (or a teammate joining the project later) would want answered. Capture
as we go; the trick with docs is writing them the moment something's
fresh, not three months later when I've forgotten the details.

If what I'm describing is more "I want to do X" than "I want to remember
how X works," capture it as an idea or a task instead — those are for
action, this is for memory.

When we've got the doc, show it back to me so I can read it. Then mark
me done — or delete me, I'm not precious.
`

// StartupOnboardingItems returns the four onboarding seed items that ship
// in a fresh `startup` workspace. The order matters: items are inserted in
// this slice's order before conventions/playbooks, so the workspace-scoped
// item_number sequence lands at IDEA-1 / PLAN-2 / TASK-3 / DOC-4.
//
// Only valid for templates that ship the canonical software defaults
// (Ideas, Plans, Tasks, Docs collections). Scrum and product templates use
// different collection sets and need their own onboarding bodies — see
// PLAN-1131 follow-ups.
func StartupOnboardingItems() []SeedItem {
	return []SeedItem{
		{
			CollectionSlug: "ideas",
			Title:          "Welcome — let's get this place set up",
			Content:        ideaOnboardingBody,
			Fields:         `{"status":"new"}`,
		},
		{
			CollectionSlug: "plans",
			Title:          "A plan I haven't written yet",
			Content:        planOnboardingBody,
			Fields:         `{"status":"planned"}`,
		},
		{
			CollectionSlug: "tasks",
			Title:          "A task I haven't named yet",
			Content:        taskOnboardingBody,
			Fields:         `{"status":"open"}`,
		},
		{
			CollectionSlug: "docs",
			Title:          "A doc I haven't written yet",
			Content:        docOnboardingBody,
			Fields:         `{"status":"draft"}`,
		},
	}
}
