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
	if (type === 'relation') return Boolean(row.collection);
	return COLLECTABLE_TYPES.has(type);
}
