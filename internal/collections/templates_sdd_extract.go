package collections

import "encoding/json"

// extractSpecsPlaybookBody is the de-personalized body of the seeded
// `extract-specs` playbook (IDEA-2527) — the brownfield differentiator vs.
// greenfield-only SDD tools (spec-kit, Kiro), which assume you start from
// the spec. This one recovers specs FROM a living codebase.
//
// Design decisions settled with Dave (IDEA-2527 comment 4): (1) subsystem
// decomposition is proposed by the agent first, with exactly ONE human
// checkpoint before any specs get written — not a spec-per-file sweep with
// no oversight; (2) extraction doubles as an audit — extracted specs
// describe OBSERVED behavior with provenance marked, and the human review
// that flips draft→approved is explicitly where "that behavior is actually
// a bug" gets caught, not a rubber stamp; (3) incremental/on-demand by
// default — extract the subsystem you're about to touch, coverage grows
// along the path of real work, never a whole-codebase sweep unless asked.
const extractSpecsPlaybookBody = `Recover specs from existing, working code instead of writing them forward.
This is the brownfield entry point spec-first tools don't have: point it
at a subsystem and it proposes specs that describe what the code actually
does, with provenance marked, ready for human review to catch anything
that turns out to be a bug rather than intended behavior.

## Arguments

- ` + "`target`" + ` (optional, string) — a path, package, module, or subsystem description (e.g. ` + "`internal/server/handlers_workspaces.go`" + `, "the auth flow", "webhook delivery"). Since ` + "`target`" + ` is optional, the strict CLI/MCP path requires it as key=value, not positional: ` + "`pad playbook run extract-specs target=\"<subsystem hint>\"`" + `. A bare ` + "`/pad extract-specs`" + ` invocation (no target) is a designed flow too — the agent should ask what area to extract from — never default to the whole codebase.
- ` + "`dry-run`" + ` (flag, default=false) — propose the subsystem map and draft specs but don't create anything. Use to iterate before committing.

## Scope discipline — incremental, never a sweep

This playbook extracts specs for ONE subsystem at a time, chosen because
someone is about to touch it or wants it documented — not the whole
codebase in one run. Coverage grows along the path of real work. If asked
to "extract specs for everything," push back: propose starting with the
highest-value or most-about-to-change subsystem instead, and offer to run
this playbook again later for the next one.

## Pre-flight

1. **Resolve the target.** If ` + "`target`" + ` is a path, confirm it exists. If it's a description, do a quick search (` + "`grep`" + `/glob, or ` + "`pad item search`" + ` for anything already tracked about it) to ground it in real code before proposing anything.
2. **Check for existing specs in the area.** ` + "`pad item list specs --field area=<area> --format json`" + ` (and a broader ` + "`pad item search`" + ` — the ` + "`area`" + ` field is free text and might not match exactly). Don't re-extract what's already covered; note what exists and extend or skip it.

## Conversation

### 1. Propose the subsystem map

Read the target code and produce a map of the behavioral subsystems
within it — the units of behavior a spec would naturally describe (not
necessarily one-per-file; group by what the code DOES, not how it's
organized on disk). For each proposed subsystem:

- A one-line description of what it does
- The primary entry points (functions, handlers, endpoints)
- Rough confidence: is this a clean, well-scoped unit of behavior, or does it bleed into neighboring concerns?

` + "```" + `
Subsystem map for <target>:

  1. <Subsystem A> — <one-line description>
     Entry points: <...>
  2. <Subsystem B> — <one-line description>
     Entry points: <...>
  ...
` + "```" + `

### 2. Human checkpoint — the ONE required stop

Present the map and STOP. Ask the user which subsystems to extract specs
for (all of them, a subset, or none — their call). Do not draft any specs
before this checkpoint clears. This is the single required pause in the
whole playbook; everything before it is exploration, everything after it
proceeds on the approved subset without further per-subsystem check-ins
unless the user asks for more granularity.

### 3. Draft specs for the approved subsystems

For each approved subsystem, read its code, tests, and any existing docs/
comments closely, then draft a spec using the same skeleton ` + "`/pad spec`" + `
produces (Context / Goals / Non-goals / Specified behavior / Acceptance
criteria / Open questions) — with two extraction-specific differences:

- **Provenance marker, always.** Every extracted spec's Context section opens with:

  ` + "```markdown" + `
  > **Extracted from observed behavior** in ` + "`<paths>`" + ` on <date>. This spec describes what the code currently does, not independently-verified design intent — review carefully; if something here reads as a bug, that's the point of review, not a drafting error.
  ` + "```" + `

- **Specified behavior describes what IS, not what SHOULD BE.** Write MUST/SHOULD statements from the code's actual observed behavior — including edge cases, error handling, and anything that looks unintentional. Resist the urge to "clean up" the described behavior while drafting; that's the reviewer's call to make explicitly, not something to bake into the extraction silently.
- **Acceptance criteria come from tests where they exist.** An existing test that exercises a behavior is strong evidence for an AC; where there's no test, derive the criterion from reading the code directly and flag it as untested in Open questions — that's useful signal for follow-up work.
- **Open questions capture anything that looks off.** If something in the observed behavior looks like a bug, an inconsistency, or an unintentional side effect, put it in Open questions explicitly rather than quietly specifying it as correct. This is the "extraction doubles as an audit" property — the agent's job during extraction is to notice and flag, not to adjudicate.

Create each as ` + "`draft`" + `:

` + "```bash" + `
pad item create specs "<title>" --field version="v1" --field area="<subsystem>" --status draft --stdin <<EOF
<assembled body with provenance marker>
EOF
` + "```" + `

**If ` + "`dry-run`" + ` is true, present the drafts in chat instead and stop — don't create anything.**

### 4. Report

` + "```" + `
Extracted N specs from <target>:
  - SPEC-X — <subsystem A>
  - SPEC-Y — <subsystem B>
  ...

All created as draft. Recommend review soon — extraction can misread
intent as bug or vice versa, and draft specs don't gate anything yet.
` + "```" + `

## Philosophy

- **One checkpoint, not zero and not many.** No oversight before writing specs risks a pile of drafts nobody asked for; a checkpoint per subsystem turns this into an interview no faster than writing specs by hand. One map, one approval, then go.
- **Extraction is an audit, not just documentation.** The value isn't only "now this subsystem has a spec" — it's that drafting one forces close reading, and close reading surfaces behavior that's actually wrong. Draft→approved review is where that gets caught; don't let extraction feel like a formality that skips review.
- **Describe what is, mark what's uncertain.** An extracted spec's job is to be an honest snapshot with provenance, not a polished aspirational document. Silently upgrading observed quirks into "intended design" defeats the audit value.
- **Incremental, always.** Coverage that grows along the path of real work stays accurate, because it's revisited as code changes. A whole-codebase sweep produces a pile of specs that goes stale immediately and covers areas nobody's about to touch.
`

// extractSpecsPlaybookArguments mirrors the body's `## Arguments` section.
var extractSpecsPlaybookArguments = []map[string]any{
	{
		"name":        "target",
		"type":        "string",
		"description": "A path, package, module, or subsystem description to extract specs from. Empty means ask — never default to the whole codebase.",
	},
	{
		"name":        "dry-run",
		"type":        "flag",
		"default":     false,
		"description": "Propose the subsystem map and draft specs but don't create anything.",
	},
}

// ExtractSpecsPlaybook returns the seeded `extract-specs` playbook for the
// spec template. invocation_slug=`extract-specs` routes
// `/pad extract-specs <target>` here.
func ExtractSpecsPlaybook() SeedPlaybook {
	fields := map[string]any{
		"status":          "active",
		"trigger":         "manual",
		"scope":           "all",
		"invocation_slug": "extract-specs",
		"arguments":       extractSpecsPlaybookArguments,
	}
	encoded, _ := json.Marshal(fields)
	return SeedPlaybook{
		Title:   "Extract specs from the codebase",
		Content: extractSpecsPlaybookBody,
		Fields:  string(encoded),
	}
}
