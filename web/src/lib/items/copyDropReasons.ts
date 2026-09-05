/**
 * The copy/move dialog's drop-reason vocabulary, and the sentences shown for
 * each one.
 *
 * EXTRACTED FROM `CopyItemDialog.svelte` (IDEA-2894). It lived inline in the
 * component, unexported, with no test file for the dialog at all — and two
 * separate review rounds found defects in it:
 *
 *   - round 12: the UI asserted NON-EXISTENCE from `not_found`, which the
 *     server also emits for a target the caller merely cannot see. Telling
 *     those apart is the existence oracle the server works to prevent.
 *   - round 18: `referent_not_portable` read "it points at something in the
 *     source workspace", claiming both existence and location for a reason
 *     emitted WITHOUT resolving the target — and which `github_pr` also
 *     reaches, where the referent is in no workspace at all.
 *
 * Neither fix was pinned by anything. A third defect of the same shape would
 * have been found the same way — by a reviewer happening to read it — or not
 * at all.
 */

/**
 * Every drop reason the server can put in a preflight `dropped[].reason` or a
 * copy's dropped-field report.
 *
 * KEPT IN THE SAME ORDER as the Go declarations so a diff of one against the
 * other is readable. The first five are the migrate-level reasons in
 * `handlers_items_copy_preflight.go`; the last five are the relation-level
 * `RelationIssueReason` constants in `internal/store/relation_referents.go`.
 *
 * This list is the CONTRACT the completeness test checks the map against. It
 * is duplicated from Go rather than generated, so it can go stale — which is
 * exactly what happened when BUG-2674 added `referent_not_portable`
 * server-side and nothing here learned it, and the reason rendered through the
 * fallback as a raw enum string. The test cannot catch a reason added to Go
 * and not added here; what it CAN catch is a reason added here without a
 * sentence, and it makes this list the one place to update.
 */
export const COPY_DROP_REASONS = [
	'no_target_field',
	'incompatible_type',
	'undeclared_source_field',
	'assignee_not_a_member',
	'agent_role_not_portable',
	'referent_not_portable',
	'not_found',
	'wrong_collection',
	'target_missing',
	'invalid_shape',
] as const;

export type CopyDropReason = (typeof COPY_DROP_REASONS)[number];

const MESSAGES: Record<CopyDropReason, string> = {
	no_target_field: 'no matching field in the destination',
	incompatible_type: 'the destination field has a different type',
	undeclared_source_field: 'not declared by this item’s own collection',
	assignee_not_a_member: 'the assignee is not a member of the destination',
	agent_role_not_portable: 'agent roles are workspace-local',

	// NEUTRAL WORDING (codex round 18). Emitted for EVERY carried
	// cross-workspace relation WITHOUT resolving the target, so the value may
	// name a live item, a deleted one, one the caller cannot see, or nothing
	// at all — and `github_pr` reaches it too, where the referent is not in
	// any workspace. Any sentence about WHERE the target is, or THAT it
	// exists, is a claim the response does not support.
	referent_not_portable: 'this reference cannot be carried to the destination',

	// NEUTRAL WORDING (codex round 12). `not_found` is what the server
	// collapses a HIDDEN target to as well as a missing one — telling them
	// apart is the existence oracle the collapse exists to prevent — so a
	// sentence asserting non-existence is both wrong for half the cases and a
	// claim the response cannot support.
	not_found: 'the item it refers to could not be found',

	wrong_collection: 'it refers to an item outside the field’s collection',

	// Covers both "no target collection declared" and "the declared collection
	// is not in this workspace"; the first wording named only the former and
	// misdiagnosed the latter.
	target_missing: 'the field has no valid collection to link to',

	invalid_shape: 'the destination field’s default is not a valid reference',
};

/**
 * The sentence to show for a drop reason.
 *
 * An UNKNOWN reason returns the raw string. That is deliberate and is not a
 * fallback to be tidied away: a reason this build has never heard of means the
 * server is ahead of the client, and showing the enum is more honest than
 * inventing a sentence for it or hiding the row entirely. The completeness
 * test exists so that the known vocabulary never reaches this branch.
 */
export function copyDropReasonMessage(reason: string): string {
	return MESSAGES[reason as CopyDropReason] ?? reason;
}
