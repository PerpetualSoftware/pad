import type { FieldDef } from '$lib/types';

/**
 * Editable view of a FieldDef used by the collection modals.
 *
 * Mirrors FieldDef but:
 * - `options` is always an array (never undefined) for easier binding.
 * - `originalOptions` snapshots the original option order/values so we can
 *   build rename migrations on save.
 * - `terminalOptions` is always an array for easier binding (mirrors
 *   FieldDef.terminal_options).
 *
 * Used for both existing fields (loaded from a saved collection) and new
 * fields (not yet persisted). For new fields, `key` is typically empty until
 * save, at which point the parent modal derives it from `label`.
 */
export interface EditableField {
	key: string;
	label: string;
	type: FieldDef['type'];
	options: string[];
	originalOptions: string[];
	terminalOptions: string[];
	required?: boolean;
	computed?: boolean;
	collection?: string;
	suffix?: string;
	default?: unknown;
}

export const FIELD_TYPES: FieldDef['type'][] = [
	'text',
	'number',
	'select',
	'multi_select',
	'date',
	'checkbox',
	'url',
	'relation'
];

/** Create an empty EditableField for a new (unsaved) field. */
export function blankField(): EditableField {
	return {
		key: '',
		label: '',
		type: 'text',
		options: [],
		originalOptions: [],
		terminalOptions: []
	};
}
