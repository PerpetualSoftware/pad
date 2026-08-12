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
recon couldn't fill. Nothing gets created until the user approves in chat.

## Arguments

- ` + "`target`" + ` (required, string OR ref) — what to spec. Accepts either:
  - **A free-text topic** (e.g. "rate limiting for the webhook endpoint") — the normal "draft a new spec" path.
  - **An IDEA-ref or BUG-ref** (e.g. ` + "`IDEA-12`" + `, ` + "`BUG-88`" + `) — the graduation path: an idea or bug becomes the spec's source material instead of a blank slate. Only refs that resolve to an Ideas-like or Bugs-like collection actually graduate (see Dispatch below) — anything else is treated as recon context for a new-topic draft.
- ` + "`collection`" + ` (optional, string, default=specs) — collection to create the spec in.

## Dispatch — new topic vs. graduate an existing ref

Detect which mode from ` + "`target`" + `:

- **Looks like a ref** (matches ` + "`^[A-Z]+-\\d+$`" + `) → resolve it (` + "`pad item show <target> --format json`" + `) and check its collection before deciding anything else:
  - **Collection is Ideas-like or Bugs-like** (slug ` + "`ideas`" + ` / ` + "`bugs`" + `, or — if the workspace renamed it — check ` + "`pad collection list --format json`" + ` for the collection the workspace actually uses for ideas/bugs) → **graduation mode**. The source item's body AND its full comment trail (` + "`pad item comments <target>`" + `) are the recon material — comment trails often carry the decision history that never made it into the body (design discussions, corrections, settled trade-offs). Read all of it before drafting.
  - **Any other collection** (e.g. a Task, a Doc) → **NOT graduation**. Ref resolution still succeeded, but the item isn't an Idea or a Bug, so nothing about it gets a status flip. Tell the user (e.g. "TASK-7 is a task, not an idea or bug — I'll use it as background context for a new spec instead of graduating it"), then fall through to new-topic mode using the item's body as extra recon material.
- **Doesn't look like a ref, or doesn't resolve** → **new-topic mode**. Recon comes from the codebase and a workspace search instead of a source item.

## Pre-flight

1. **Confirm the target collection exists.** ` + "`pad collection list --format json`" + `. If ` + "`collection`" + ` doesn't resolve, ask which one to use.
2. **Graduation mode: load the source item fully.** ` + "`pad item show <target> --format markdown`" + ` plus ` + "`pad item comments <target>`" + `. Read everything — this is the recon, not a formality.
3. **New-topic mode: search for overlap.** ` + "`pad item search \"<target>\" --format json`" + ` across specs, ideas, and docs. If a closely related spec already exists (even a draft), tell the user and ask whether to extend it instead of starting fresh.

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

If the user approved outright in step 4, follow immediately with:

` + "```bash" + `
pad item update <new-spec-ref> --status approved --comment "Approved in draft conversation."
` + "```" + `

If the user wants to circulate the draft for others to review before
committing, leave it at ` + "`in-review`" + ` instead and stop here — the
approval step happens later, either by rerunning this playbook or a plain
` + "`pad item update <ref> --status approved`" + `.

### 6. Graduate the source item (graduation mode only — see Dispatch)

Flip the source IDEA or BUG to whatever its OWN collection schema defines
as terminal — don't hardcode a status name, collections define their own
vocabulary (` + "`pad collection list --format json`" + ` and check the
source item's collection's status field for ` + "`terminal_options`" + `; an
Ideas collection typically uses ` + "`implemented`" + `, but read the actual
schema rather than assuming). If more than one terminal option could
apply, ask the user which one fits. Always include a comment citing the
new spec:

` + "```bash" + `
pad item update <source-ref> --status <its-actual-terminal-status> --comment "Graduated into <new-spec-ref>."
` + "```" + `

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
		"description": "What to spec. Free-text topic for a new draft, or an IDEA-ref/BUG-ref to graduate an existing item into a spec (only if it resolves to an Ideas-like or Bugs-like collection — otherwise it's used as recon context instead).",
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
