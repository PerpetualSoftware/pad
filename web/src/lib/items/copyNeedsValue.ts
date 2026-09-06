import type { ItemCopyPreflightNeedsValue } from '$lib/types';

/**
 * Types the copy dialog can safely collect a value for — the set `FieldEditor`
 * implements as real typed controls.
 *
 * Everything else is NOT rendered as a text box. `json` is deliberately
 * read-only in FieldEditor (a plain text input would store the string "[]"
 * where an array belongs), and `multi_select` falls through FieldEditor's TEXT
 * fallback, which yields a string where the server requires `[]any`. That last
 * one is the dangerous case: enterable and silently invalid.
 */
export const COLLECTABLE_TYPES = new Set([
	'text',
	'number',
	'select',
	'date',
	'checkbox',
	'url',
]);

/**
 * Whether a `needs_value` row can be given a value in the copy dialog.
 *
 * `relation` is collectable ONLY IF THE ROW NAMES ITS TARGET COLLECTION
 * (TASK-2869). FieldEditor's relation branch is gated on `wsSlug` AND
 * `field.collection`; with either missing it renders the non-editable state, so
 * offering the row without a collection produces a control that cannot be
 * filled and a Confirm that cannot be satisfied.
 *
 * A row missing `collection` is therefore treated exactly as an uncollectable
 * TYPE — it lands in the blocked list and the user is told which field and why,
 * instead of meeting a dead input, or worse an unscoped picker offering
 * SOURCE-workspace items the copy cannot use.
 *
 * EXTRACTED RATHER THAN LEFT INLINE, for the reason IDEA-2894 established one
 * unit earlier: logic living unexported inside `CopyItemDialog.svelte` cannot
 * be tested, and the mutant that made `relation` unconditionally collectable
 * survived every suite in the repo while it lived there.
 */
export function isCollectable(row: ItemCopyPreflightNeedsValue): boolean {
	const type = row.type ?? 'text';
	if (type === 'relation') {
		if (!row.collection) return false;
		// IDEA-2899. Naming a target is not having one: the slug can name a
		// collection that has been deleted, or one this caller cannot read.
		// Either way the picker mounts and returns nothing, and the row stays
		// out of `blockedFields`, so Confirm is disabled with only the generic
		// required-field message — a value is missing and nothing says no value
		// is reachable.
		//
		// STRICT `=== true`, not truthiness. The field is ABSENT when the target
		// is fine AND absent from a server that predates it, so absence must
		// read as "no information" rather than as a value.
		//
		// SAID PLAINLY BECAUSE A MUTANT PROVED IT: over the domain this field's
		// TYPE admits — `boolean | undefined` — `!row.collection_unavailable`
		// is EQUIVALENT, and swapping it in kills no test and breaks nothing.
		// It is not a defect and no test is owed for it. The strict form is
		// kept for a reason about the next edit rather than this one: it states
		// the contract in the code, so the inverse spelling
		// (`collection_available`, which WOULD block every row against a server
		// that omits it) reads as the mistake it is.
		return row.collection_unavailable !== true;
	}
	return COLLECTABLE_TYPES.has(type);
}

/**
 * Why a row cannot be collected, for the message the dialog shows.
 *
 * The two reasons need DIFFERENT copy and, more importantly, different advice.
 * A `json` or `multi_select` field genuinely cannot be typed into this dialog
 * safely, and the CLI can set it — so pointing at `pad item copy --field` is
 * real help. An unavailable relation TARGET is not like that: the CLI runs as
 * the same user against the same referent validation, so the command the dialog
 * would print gets refused for the same reason. Offering it sends the user to
 * do work that cannot succeed.
 */
export type UncollectableReason = 'type' | 'unavailable_target';

export function uncollectableReason(
	row: ItemCopyPreflightNeedsValue
): UncollectableReason | null {
	if (isCollectable(row)) return null;
	const type = row.type ?? 'text';
	// A relation naming a target the caller cannot use — as opposed to one
	// naming no target at all, which is the TASK-2869 case and stays a
	// type-shaped failure: there is nothing to point the user at.
	if (type === 'relation' && row.collection && row.collection_unavailable === true) {
		return 'unavailable_target';
	}
	return 'type';
}
