package collections

import "encoding/json"

// verifyPlaybookBody is the de-personalized body of the seeded `verify`
// playbook (IDEA-2527): walk a spec's acceptance criteria against the
// actual diff/behavior and report per-criterion pass/fail, offering to
// flip the spec to implemented once everything passes.
const verifyPlaybookBody = `Walk a spec's acceptance criteria against the current diff or running
behavior and report which ones actually hold. This is the "verify-against-
spec" step SDD tools promise and most don't deliver — it's what makes
"approved" mean something more than "reviewed once."

## Arguments

- ` + "`target`" + ` (required, ref) — the SPEC-N to verify.
- ` + "`diff-only`" + ` (flag, default=false) — check acceptance criteria against the current diff (uncommitted changes + the current branch vs. its base) only, skip checking already-merged behavior. Useful mid-implementation; the default (false) checks the full current state of the codebase, useful for periodic audits.

## Pre-flight

1. **Resolve the target.** ` + "`pad item show <target> --format markdown`" + `. If it doesn't resolve or isn't in the specs collection, stop and report.
2. **Confirm it has acceptance criteria.** Parse the ` + "`## Acceptance criteria`" + ` section. If there are none (or the spec is still ` + "`draft`" + `), tell the user there's nothing to verify yet and stop — don't invent criteria to check. Otherwise note the spec's current status — you'll need it in Resolve to decide whether an all-pass result can actually flip the spec, since the flip is gated on approval, not just on criteria holding.
3. **Load the relevant code.** If ` + "`diff-only`" + ` is set, ` + "`git diff <base>...HEAD`" + ` (or ` + "`git status`" + ` + ` + "`git diff`" + ` for uncommitted work). Otherwise, use the spec's ` + "`area`" + ` field and Specified behavior section to identify which code paths to check directly.

## Verification

For each AC-N in order:

1. **Read the criterion.** Restate in your own words what it claims and what evidence would confirm or refute it.
2. **Find the evidence.** Look at the actual code, tests, or behavior — not the PR description or commit message, which describe intent, not outcome. Run tests if the criterion is testable and a test exists; read the implementation directly if it isn't.
3. **Judge pass / fail / can't-tell.**
   - **Pass** — evidence directly supports the criterion being true.
   - **Fail** — evidence contradicts it, or the described behavior doesn't exist.
   - **Can't-tell** — no way to check mechanically or by reading (e.g. the criterion depends on a live environment you don't have access to). Report this honestly rather than guessing a verdict.
4. **Record specifics.** For a fail, cite the file/line or behavior that contradicts the criterion. For a pass, cite what confirmed it (test name, code location). Vague verdicts aren't useful to the next reader.

## Report

` + "```" + `
Verification of SPEC-N — "<title>":

  AC-1: <restatement>                    ✅ PASS  — <evidence>
  AC-2: <restatement>                    ❌ FAIL  — <what's wrong, where>
  AC-3: <restatement>                    ❓ CAN'T TELL — <why>

N/M criteria pass.
` + "```" + `

## Resolve

- **All criteria pass:** what happens next depends on the spec's current status — the flip to ` + "`implemented`" + ` is gated on approval, not just on the criteria holding:
  - **Status is ` + "`approved`" + `:** offer to flip the spec's status to ` + "`implemented`" + `:

    ` + "```bash" + `
    pad item update <target> --status implemented --comment "Verified: all N acceptance criteria pass. <one-line summary>."
    ` + "```" + `

    Ask before flipping — verification confirms the spec's claims hold, but the user may still want to hold status for other reasons (a pending rollout, a follow-up spec that supersedes part of this one, etc.).
  - **Status is already ` + "`implemented`" + ` (a re-verify):** nothing to flip — report that re-verification confirms the criteria still hold.
  - **Status is ` + "`in-review`" + `:** don't flip. Report the pass, but tell the user the spec isn't approved yet — the criteria holding doesn't substitute for approval. Point them at finishing the review (` + "`pad item update <target> --status approved`" + ` once whoever's reviewing signs off).
  - **Status is ` + "`superseded`" + `:** don't flip. Tell the user this spec was superseded — passing criteria on a superseded spec isn't actionable on its own. Point them at whatever spec replaced it (check the spec's body/comments for a reference, or search for one) since that's the one that should be verified and implemented going forward.

- **Some criteria fail:** this is spec-code drift — per the workspace's ` + "`drift-is-a-bug`" + ` convention, decide which side is wrong. Either:
  - the code needs to change to satisfy the criterion (the common case — report the failures and let the user decide whether to fix now or track as a task), or
  - the criterion itself was wrong or has been overtaken by a later decision (rarer — this needs a spec change, which per the workspace's supersede-don't-mutate convention means re-review or a new spec, not silently editing the failing criterion away).

  Don't auto-fix code or auto-edit the spec — report the drift and let the user pick a side.

- **Can't-tell criteria:** report them separately; they neither block nor confirm the verification. If there are more than a couple, that's a signal the criteria weren't written to the verifiability rule (` + "`/pad spec`" + `'s discipline) and might be worth tightening on the next spec revision.

## Philosophy

- **Evidence, not intent.** A PR description saying "implements AC-2" isn't evidence AC-2 holds — the actual diff/behavior is.
- **Drift is a decision, not an error to suppress.** When a criterion fails, that's real information about where the spec and the code disagree — surface it, don't paper over it.
- **Honest about the limits of static/agent checking.** Some criteria genuinely can't be verified without a live environment or human judgment. Say so rather than forcing a pass/fail verdict.
- **Verification earns the status flip.** Don't move a spec to ` + "`implemented`" + ` on the strength of a PR merging — move it on the strength of its criteria actually holding.
`

// verifyPlaybookArguments mirrors the body's `## Arguments` section.
var verifyPlaybookArguments = []map[string]any{
	{
		"name":        "target",
		"type":        "ref",
		"required":    true,
		"description": "The SPEC-N to verify.",
	},
	{
		"name":        "diff-only",
		"type":        "flag",
		"default":     false,
		"description": "Check acceptance criteria against the current diff only, instead of the full codebase.",
	},
}

// VerifyPlaybook returns the seeded `verify` playbook for the spec
// template. invocation_slug=`verify` routes `/pad verify SPEC-N` here.
func VerifyPlaybook() SeedPlaybook {
	fields := map[string]any{
		"status":          "active",
		"trigger":         "manual",
		"scope":           "all",
		"invocation_slug": "verify",
		"arguments":       verifyPlaybookArguments,
	}
	encoded, _ := json.Marshal(fields)
	return SeedPlaybook{
		Title:   "Verify a spec",
		Content: verifyPlaybookBody,
		Fields:  string(encoded),
	}
}
