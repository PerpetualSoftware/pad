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
//   - They grow two ways. A trigger or scope the convention/playbook
//     LIBRARY uses is added by the server the first time a write uses
//     it, and reported (BUG-3446, collections.LibraryOptionVocabulary):
//     the onboard playbook used to say nothing about widening, so every
//     triggered library convention was refused here. A word the library
//     does not use (hiring's `on-candidate-advance`, research's
//     `on-experiment-run`) is still added deliberately, by the owner,
//     via `pad collection update` (TASK-1510); the onboard playbook's B3
//     says how. Keeping the seed minimal still means only the words a
//     workspace actually uses end up listed.

var (
	BlankConventionTriggers = []string{"always"}
	BlankConventionScopes   = []string{"all"}
	BlankPlaybookTriggers   = []string{"manual"}
	BlankPlaybookScopes     = []string{"all"}
)
