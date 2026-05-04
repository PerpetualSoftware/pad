package collections

// Onboarding seed items for new product-template workspaces.
//
// Same mechanism as the startup + scrum onboarding seeds: one seed
// item per user-facing collection, agent-invocable, written
// first-person from the workspace owner's future self. Any of
// FEAT-1 / FB-2 / ROAD-3 / DOC-4 is a viable entry point for
// `/pad let's discuss <REF>`.
//
// Status of the post-signup hint that names the primary entry: as of
// this PR (TASK-1149) the CLI hint and dashboard banner are still
// hardcoded to IDEA-1 — they were shipped against the startup template
// only (PR #403). TASK-1150 makes those template-aware so a fresh
// product workspace surfaces "use pad to get FEAT-1" rather than
// IDEA-1. Until TASK-1150 lands, the bodies are still reachable via
// direct fetch (`pad item show FEAT-1`) but the dashboard banner
// doesn't promote them.
//
// Bodies and design rationale: DOC-1153 (PLAN-1146).
//
// Schema-valid terminal verbs per collection (locked at design time
// to prevent the round-1 regression on PR #402):
//   - Features      → status `shipped`
//   - Feedback      → status `shipped` (closed when the change ships)
//   - Roadmap Items → status `completed`
//   - Docs          → status `archived`

const productFeatureOnboardingBody = `# Welcome — let's start shaping what we're going to ship

I just made this product workspace. It's where I'm going to keep
track of what we're building: features moving through the build
funnel, feedback we're collecting from users, roadmap commitments,
and docs.

If you're reading this, it's because I asked you to. I'd like your
help getting started — not with a tutorial, just by talking through
what we're actually building and capturing it here as we go.

## What I'd find useful

Ask me what we're working on. Listen for the shape of the product
— what we just shipped, what's in research, what's queued up, where
the user signals are pulling us. As things come up, capture them
here. Features for things we're building, feedback for signals
worth remembering, roadmap items for the bigger bets we're placing,
docs when a decision needs to outlive the conversation.

Use the actual product, not a toy example. Don't ask permission for
every little thing — narrate what you're capturing as you go.

When we've got enough here to be useful, run ` + "`pad project dashboard`" + `
so I can see the build funnel, and point me at the web UI. Then
mark me shipped.

## A few things to know about Pad

- Items have IDs like FEAT-7, FB-3, ROAD-2 — that's how I'll
  refer to them.
- ` + "`pad project next`" + ` will tell me what to work on.
- ` + "`pad project dashboard`" + ` shows the state of the build funnel.
- ` + "`pad item search \"term\"`" + ` finds anything by content.
- ` + "`pad <anything>`" + ` in a /pad-aware agent session is how I'll talk
  to you in the future.

## If I've already done this before

If you look around and see I already have features, feedback,
roadmap items of my own — I'm not new, I just re-opened you. Don't
redo any of this. Recap what's in the dashboard and let me get
back to work.
`

const productFeedbackOnboardingBody = `# A piece of feedback I haven't captured yet

This is where user signals live — quotes, tickets, recurring
complaints, requests we want to remember. Feedback closes its
lifecycle when the change it inspired actually ships.

## What I'd find useful

If you're with me through this item, I've probably just heard
something useful from a user — a sales call quote, a support
ticket, a pattern across multiple complaints. Help me capture it
without losing the texture: who said it (or which segment), what
they said in their words, what they wanted, what context they were
in. Don't editorialize — the value of feedback is the unfiltered
signal.

If the feedback is concrete enough to act on right now, fine —
it'll surface as a feature. If it's a recurring theme worth a
bigger bet, it might fold into a roadmap item. Capture the signal
first; sort the rest out later.

When the change motivated by this feedback actually goes out, mark
me shipped. Or delete me — I'm not precious.
`

const productRoadmapOnboardingBody = `# A roadmap commitment I haven't placed yet

This is where the bigger bets live — quarter-or-larger commitments
that organize feature work into themes. Roadmap items aren't
features; they're frames around features.

## What I'd find useful

If you're with me through this item, I'm probably thinking about a
chunk of work bigger than one feature. A theme ("self-serve
onboarding"), an initiative ("cut p99 latency by half"), a strategic
bet. Help me think through it: what's the bet, why now, what
features ladder up to it, what would success look like in plain
language, what we're explicitly choosing NOT to do at this level.

If what I'm describing fits in a single PR or a single feature,
that's a feature, not a roadmap item. If it's a vague aspiration
without enough shape to commit, it might be earlier than that — a
doc, a feedback theme to watch.

When the work actually lands, mark me completed. Or delete me, I'm
not precious.
`

const productDocOnboardingBody = `# A doc I haven't written yet

This is where I write things down that I'll need later —
architecture decisions, postmortems, the user-research synthesis I
want available when we're spec'ing the next feature.

## What I'd find useful

If you're with me through this item, I probably want to write
something down so I don't have to figure it out again next quarter.
Help me figure out what kind of doc this is — a decision record, a
research synthesis, a runbook, a postmortem — and what should go in
it. Capture as we go; the trick with docs is writing them the
moment something's fresh, not three quarters later when the context
has evaporated.

If what I'm describing is more "we should ship this" than "we should
remember this," capture it as a feature or feedback instead. This
is for memory.

When we've got the doc, show it back to me. Then archive me — or
delete me, I'm not precious.
`

// ProductOnboardingItems returns the four onboarding seed items that
// ship in a fresh `product` workspace. The order matters: items are
// inserted in this slice's order before conventions/playbooks, so
// the workspace-scoped item_number sequence lands at FEAT-1 / FB-2 /
// ROAD-3 / DOC-4. FEAT-1 is the designated primary — TASK-1150 will
// wire the post-signup hint and dashboard banner to surface it.
//
// Features is the primary entry — uncontested. Roadmap is the
// strategic frame above features; feedback is the signal source
// below. Features are the unit of building work, the natural first
// thing a product workspace's owner thinks about.
func ProductOnboardingItems() []SeedItem {
	return []SeedItem{
		{
			CollectionSlug: "features",
			Title:          "Welcome — let's start shaping what we're going to ship",
			Content:        productFeatureOnboardingBody,
			Fields:         `{"status":"proposed"}`,
		},
		{
			CollectionSlug: "feedback",
			Title:          "A piece of feedback I haven't captured yet",
			Content:        productFeedbackOnboardingBody,
			Fields:         `{"status":"new"}`,
		},
		{
			CollectionSlug: "roadmap-items",
			Title:          "A roadmap commitment I haven't placed yet",
			Content:        productRoadmapOnboardingBody,
			Fields:         `{"status":"planned"}`,
		},
		{
			CollectionSlug: "docs",
			Title:          "A doc I haven't written yet",
			Content:        productDocOnboardingBody,
			Fields:         `{"status":"draft"}`,
		},
	}
}
