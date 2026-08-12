package collections

import "encoding/json"

// specPlaybookBody is the de-personalized body of the seeded `spec`
// playbook (IDEA-2527). Draft-first + ask-only-gaps interview design
// settled with Dave: recon → four slots → full draft in chat →
// conversational approval → hand off to decompose. Two discipline rules
// carried through from the design record: every MUST maps to ≥1 AC (a
// self-audit before presenting, not a question asked of the user), and the
// verifiability rule (if you can't say how an agent would check it, it's a
// goal, not a criterion).
//
// Authoring principle (Dave, IDEA-2527 comment 5, applies to all three SDD
// playbook bodies): generic playbooks ship judgment-triggers, not
// unconditional gates. The MUST→AC audit is mechanical enough to be a real
// gate; asking the user a fixed set of interview questions regardless of
// what's already known is not — so the four slots below are framed as
// "confirm only where recon leaves a genuine gap," not as a mandatory
// questionnaire.
const specPlaybookBody = `Draft a spec — either from a free-text topic or by graduating an existing
Idea or Bug into one. Draft-first: the agent does the recon and produces a
full draft before asking the user anything, then only asks about the gaps
recon couldn't fill. The draft is always presented in chat first — nothing
is created until the user chooses to approve outright or circulate for
review.

## Arguments

- ` + "`target`" + ` (required, string OR ref) — what to spec. Accepts either:
  - **A free-text topic** (e.g. "rate limiting for the webhook endpoint") — the normal "draft a new spec" path.
  - **An IDEA-ref or BUG-ref** (e.g. ` + "`IDEA-12`" + `, ` + "`BUG-88`" + `) — the graduation path: an idea or bug becomes the spec's source material instead of a blank slate. Only refs that resolve to an Ideas-like or Bugs-like collection actually graduate (see Dispatch below) — anything else is treated as recon context for a new-topic draft.
  - **A SPEC-ref** (e.g. ` + "`SPEC-4`" + `) — resume mode: continues an earlier ` + "`/pad spec`" + ` run instead of starting a new one (e.g. finishing approval on a spec left ` + "`in-review`" + `, and completing any graduation that was left pending). Never creates a duplicate within the resolved specs collection.
- ` + "`collection`" + ` (optional, string, default=specs) — collection to create the spec in.

## Dispatch — new topic vs. graduate an existing ref

Detect which mode from ` + "`target`" + `:

- **Looks like a ref** (matches ` + "`^[A-Z]+-\\d+$`" + `) → resolve it (` + "`pad item show <target> --format json`" + `) and check its collection before deciding anything else:
  - **Collection is the target collection for this run** (the resolved ` + "`collection`" + ` argument, default ` + "`specs`" + ` — check ` + "`pad collection list --format json`" + ` if the workspace renamed it, same as the ideas/bugs check below) → **resume mode**. This isn't a new draft — it's the continuation of an earlier ` + "`/pad spec`" + ` run (see Resume below). Never creates a duplicate within the resolved specs collection for the same target.
  - **Collection is Ideas-like or Bugs-like** (slug ` + "`ideas`" + ` / ` + "`bugs`" + `, or — if the workspace renamed it — check ` + "`pad collection list --format json`" + ` for the collection the workspace actually uses for ideas/bugs) → **graduation mode**. The source item's body AND its full comment trail (` + "`pad item comments <target>`" + `) are the recon material — comment trails often carry the decision history that never made it into the body (design discussions, corrections, settled trade-offs). Read all of it before drafting.
  - **Any other collection** (e.g. a Task, a Doc) → **NOT graduation**. Ref resolution still succeeded, but the item isn't an Idea or a Bug, so nothing about it gets a status flip. Tell the user (e.g. "TASK-7 is a task, not an idea or bug — I'll use it as background context for a new spec instead of graduating it"), then fall through to new-topic mode using the item's body as extra recon material.
- **Doesn't look like a ref, or doesn't resolve** → **new-topic mode**. Recon comes from the codebase and a workspace search instead of a source item.

## Pre-flight

1. **Confirm the target collection exists.** ` + "`pad collection list --format json`" + `. If ` + "`collection`" + ` doesn't resolve, ask which one to use.
2. **Graduation mode: load the source item fully, then check for an existing graduation before creating anything.** ` + "`pad item show <target> --format markdown`" + ` plus ` + "`pad item comments <target>`" + `. Read everything — this is the recon, not a formality. Then, before drafting: does the trail already show a "Graduating into <spec-ref>" comment (written by an earlier run of this playbook's step 5)? If not, also search the specs collection (` + "`pad item search \"<target>\" --format json`" + ` scoped to specs, or list-and-scan if search misses it) for a spec whose Context section names this source — step 3 mandates that citation in graduation mode precisely so this search works, and it finds even a spec created before a crash wiped out the marker comments (see step 5's note on this). If either check finds one: this run is really a resume, not a fresh draft — switch to resume mode with that spec as the target. Repair before proceeding if needed: if the found spec is missing either side's marker comment (the crash-window case from step 5), write both now — ` + "`pad item update <found-spec-ref> --comment \"Graduated from <target> — when this spec reaches approved, flip <target> to its terminal status citing <found-spec-ref>.\"`" + ` and ` + "`pad item update <target> --comment \"Graduating into <found-spec-ref> — pending approval.\"`" + ` — so Reconcile has what it needs and the recovery claim in step 5 is actually true, not just discoverable-but-inert. Then skip straight to Resume below, running its own pre-flight first (including loading the spec's comments — Reconcile depends on them). Don't create a duplicate within the resolved specs collection for the same source.
3. **New-topic mode: search for overlap.** ` + "`pad item search \"<target>\" --format json`" + ` across specs, ideas, and docs. If a closely related spec already exists (even a draft), tell the user and ask whether to extend it instead of starting fresh.
4. **Resume mode: load the spec fully.** ` + "`pad item show <target> --format markdown`" + ` plus ` + "`pad item comments <target>`" + `. If this spec was created via graduation, step 5 leaves a graduation-link comment on it unconditionally at creation time — Reconcile (see Resume below) needs it, so load the comments before doing anything else.

## Resume — target is an existing spec

Skip the Conversation section entirely; there's nothing to draft. Two parts: adjust the spec's status per its current state (below), then always run Reconcile.

### Adjust status

- **` + "`in-review`" + `:** the normal case — resuming a draft that was circulated for review. Ask the user whether review is done and they want to approve now.
  - If yes: ` + "`pad item update <target> --status approved --comment \"Approved after review.\"`" + `.
  - If not yet: report the spec's current state (open questions, who's reviewing, etc. if known) and stop here — skip Reconcile, nothing changed.
- **` + "`draft`" + `:** review never started. Report the draft and offer the same choice step 5 originally did — approve outright or circulate for review — and handle whichever the user picks the same way step 5 does.
- **` + "`approved`" + ` or ` + "`implemented`" + `:** nothing to change — proceed straight to Reconcile below. This is deliberate: it's what makes graduation resolve even when nobody used this playbook to approve the spec (e.g. a plain manual ` + "`pad item update <target> --status approved`" + `).
- **` + "`superseded`" + `:** report that and point at whatever spec replaced it, if findable. Before stopping, check this spec's own graduation marker (loaded in pre-flight): if it names a source that's still open, don't strand it — a superseded spec shouldn't itself flip anything to terminal, but the pending graduation carries forward. Find the LIVE HEAD of the supersession chain, not just the immediate successor: if the immediate successor is itself ` + "`superseded`" + `, follow its successor, and so on — bounded (cap at ~10 hops); if the chain loops back on itself or dead-ends (a successor ref that doesn't resolve) before reaching a live spec, stop walking and treat it as no successor found. Once a live head is found, it's the target for the rest of this branch: comment it with the same "Graduated from <source-ref> — ..." marker (citing its own ref). If it's already ` + "`approved`" + ` or ` + "`implemented`" + `, don't wait for a hypothetical future rerun — run Reconcile on it right now (it's idempotent, and the source ref is already in hand). Otherwise, its own future Reconcile picks up the marker once it reaches approved. If no live head is findable (chain dead-ends, loops, or there's no successor at all), tell the user the graduation is stranded and needs manual handling. Either way, stop — skip this spec's own Reconcile.

### Reconcile (runs on every resume that reaches this point, regardless of which branch above ran)

If this spec's status is now ` + "`approved`" + ` or ` + "`implemented`" + ` AND its comments (loaded in pre-flight) name a graduation source that hasn't already reached ITS OWN terminal status: complete the graduation — flip the source item to whatever its own collection schema defines as terminal (check ` + "`pad collection list --format json`" + ` for the source's collection's status field ` + "`terminal_options`" + `; if more than one option could apply, ask the user which fits), with a comment citing this spec:

` + "```bash" + `
pad item update <source-ref> --status <its-actual-terminal-status> --comment "Graduated into <spec-ref>."
` + "```" + `

Idempotent — if the source is already at its terminal status, or there's no graduation marker at all (a non-graduation spec), there's nothing to do; continue silently. This single rule is what makes graduation resolve regardless of how this resume was reached: a circulated draft finally getting approved, a rerun after a crash mid-graduation, or a rerun after someone flipped the spec's status manually outside this playbook entirely.

If the spec is now ` + "`approved`" + `, offer the decompose hand-off (step 7).

## Conversation

### 1. Recon

Synthesize what's already known: the source item's body/comments in
graduation mode, or a quick read of the relevant code paths in new-topic
mode. This is where most of the draft comes from — the interview below
fills gaps, it doesn't start from zero.

### 2. Four slots — confirm only where recon leaves a genuine gap

Don't ask all four by rote if recon already answered them; that's exactly
the kind of unconditional gate this playbook avoids. Ask only what's
actually unclear:

- **Done-means** — what does success look like? (feeds Goals)
- **Non-goals** — what's explicitly out of scope? Only worth confirming when there's real ambiguity about the boundary — an obvious non-goal doesn't need a question.
- **Consumers** — who or what depends on this behavior? (other services, other agents, a specific UI surface)
- **Touchpoints** — what code paths or existing behavior does this spec constrain or change?

### 3. Draft the full spec in chat

Assemble the draft using the collection's content skeleton (Context /
Goals / Non-goals / Specified behavior / Acceptance criteria / Open
questions) and post the WHOLE thing in chat before creating anything.

**Graduation mode: the Context section MUST cite the source ref by ID**
(e.g. "Grew from IDEA-12"), not just describe it in prose. The skeleton's
own hint says "(if any)" because most specs aren't graduated — but for
one that is, this citation is what makes crash recovery possible at all:
it's the search key pre-flight step 2 and step 5's recovery path rely on
to find this spec by its source. A description without the ID doesn't
work as a search key.

Two discipline passes before presenting:

- **MUST→AC self-audit.** Every MUST statement in Specified behavior needs at least one AC-N that would catch a violation of it. For each orphaned MUST, either add the missing criterion or downgrade the statement to a SHOULD or a Goal — don't present a draft with untested MUSTs.
- **Verifiability rule, per criterion.** For each AC-N, ask yourself: how would an agent actually check this against a diff or running behavior? If you can't answer concretely, it doesn't belong in Acceptance criteria — move it to Goals instead. "The system should be fast" isn't checkable; "p99 latency under 200ms for the webhook endpoint" is.

Anything genuinely unresolved goes in Open questions rather than being
guessed at.

### 4. Present for conversational approval

Show the full draft. Open questions are the one thing that blocks
approval — everything else can be approved as drafted or iterated on.
If open questions remain, either resolve them right there in the
conversation or ask the user whether they're comfortable approving with
the questions still open (their call, not a hard stop).

### 5. Create the spec

` + "```bash" + `
pad item create <collection> "<title>" --field version="v1" --field area="<area>" --status draft --stdin <<EOF
<assembled body>
EOF
` + "```" + `

**Graduation mode only:** immediately record the graduation link on BOTH
items, before anything else runs — this needs to exist no matter which
branch below happens next, or whether the run gets interrupted entirely
between here and either branch completing:

` + "```bash" + `
pad item update <new-spec-ref> --comment "Graduated from <source-ref> — when this spec reaches approved, flip <source-ref> to its terminal status citing <new-spec-ref>."
pad item update <source-ref> --comment "Graduating into <new-spec-ref> — pending approval."
` + "```" + `

This shrinks the crash window but doesn't close it — a crash between
` + "`pad item create`" + ` and these two comments is still possible. It isn't
fatal: pre-flight step 2's existing-graduation check finds the spec by its
Context section on the next run — the source-ID citation step 3 mandates
in graduation mode is the search key, marker or not — AND repairs any
missing marker before resuming, so the recon check plus repair is the
actual recovery mechanism, not atomicity this playbook can't promise.

If the user approved outright in step 4, follow immediately with:

` + "```bash" + `
pad item update <new-spec-ref> --status approved --comment "Approved in draft conversation."
` + "```" + `

Then run Reconcile (see Resume above) right here — the spec just became
approved, and Reconcile is the one place graduation mechanics are
described; don't repeat them.

If the user wants to circulate the draft for others to review before
committing, move it to ` + "`in-review`" + ` instead and stop here:

` + "```bash" + `
pad item update <new-spec-ref> --status in-review --comment "Circulating for review before approval."
` + "```" + `

When review is done, rerun ` + "`/pad spec <new-spec-ref>`" + ` (or just say so
in chat) — Resume above picks up from here, including Reconcile once the
spec reaches approved.

### 6. Graduate the source item

This step is Reconcile (see Resume above), invoked here because step 5's
approve-outright branch just moved the spec to ` + "`approved`" + ` for the
first time. Graduation mechanics live in one place — Resume → Reconcile —
referenced from both call sites rather than duplicated.

### 7. Offer next steps

The natural follow-up is decomposing the approved spec into tasks —
mention ` + "`/pad decompose <new-spec-ref>`" + ` if a decompose playbook is
active. Don't decompose automatically; that's a separate approval.

## Philosophy

- **Draft-first, not interview-first.** Recon does the work; the user answers gaps, not a fixed questionnaire. Producing a full draft before asking anything respects the user's time far more than an upfront interrogation.
- **Judgment-triggers, not unconditional gates.** The MUST→AC audit and the verifiability rule are the parts worth enforcing every time — they're mechanical checks the agent can actually run. Asking a fixed set of questions regardless of what recon already established is not a discipline worth enforcing unconditionally.
- **Open questions block approval, not drafting.** A spec can be presented — and iterated on — with open questions. It just can't be approved with them dangling, unless the user explicitly accepts that risk.
- **Comment trails are recon, not decoration.** When graduating a ref, the comment trail often holds the actual reasoning behind decisions that never made it into the body. Read it before drafting, not after.
- **Graduate with an audit trail.** Flipping the source item's status without a comment loses the "why" the next reader needs.
`

// specPlaybookArguments mirrors the body's `## Arguments` section.
var specPlaybookArguments = []map[string]any{
	{
		"name":        "target",
		"type":        "string",
		"required":    true,
		"description": "What to spec. Free-text topic for a new draft, an IDEA-ref/BUG-ref to graduate an existing item into a spec (only if it resolves to an Ideas-like or Bugs-like collection — otherwise it's used as recon context instead), or a SPEC-ref to resume an earlier /pad spec run (e.g. finishing approval on a spec left in-review) — never creates a duplicate within the resolved specs collection.",
	},
	{
		"name":        "collection",
		"type":        "string",
		"default":     "specs",
		"description": "Collection to create the spec in.",
	},
}

// SpecPlaybook returns the seeded `spec` playbook for the spec template.
// invocation_slug=`spec` routes `/pad spec <target>` here.
func SpecPlaybook() SeedPlaybook {
	fields := map[string]any{
		"status":          "active",
		"trigger":         "manual",
		"scope":           "all",
		"invocation_slug": "spec",
		"arguments":       specPlaybookArguments,
	}
	encoded, _ := json.Marshal(fields)
	return SeedPlaybook{
		Title:   "Draft a spec",
		Content: specPlaybookBody,
		Fields:  string(encoded),
	}
}
