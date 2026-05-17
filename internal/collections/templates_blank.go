package collections

// templates_blank.go holds the seed vocabularies for the `blank`
// workspace template registered in templates.go. The template entry
// itself stays inline with the other templates so the registration
// list keeps a single shape; only the trigger / scope vocabularies
// live here because they're the part most likely to change (and most
// worth a focused diff when they do).
//
// Design notes — PLAN-1496 / TASK-1498 (originally IDEA-1479):
//   - The `blank` template is the minimal entry-point for the
//     /pad onboard playbook flow. Only the two SYSTEM collections
//     (Conventions, Playbooks) are seeded.
//   - Trigger / scope vocabularies are deliberately tiny: `always`
//     for conventions (universal "follow this" rule), `manual` for
//     playbooks (the seeded onboard playbook itself is
//     manual-triggered), and `all` for both scopes.
//   - The /pad onboard playbook broadens these via `pad collection
//     update` (TASK-1510) once the interview reveals the workspace's
//     actual domain — software adds `on-commit`/`on-implement`,
//     hiring adds `on-candidate-advance`, research adds
//     `on-experiment-run`, etc. Keeping the seed minimal forces that
//     conversation up-front rather than carrying baked-in assumptions.

var (
	BlankConventionTriggers = []string{"always"}
	BlankConventionScopes   = []string{"all"}
	BlankPlaybookTriggers   = []string{"manual"}
	BlankPlaybookScopes     = []string{"all"}
)
