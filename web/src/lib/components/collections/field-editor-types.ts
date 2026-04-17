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
 * - `keyTouched` is a UI-only flag (not serialized) that tracks whether the
 *   user has manually edited the key. When false (and the field is new),
 *   FieldEditor auto-derives the key from the label on every change. Once
 *   set to true, the auto-sync stops — the user has taken control.
 *
 * Used for both existing fields (loaded from a saved collection) and new
 * fields (not yet persisted). For existing fields the `key` is frozen;
 * for new fields the parent modal uses whatever the user ends up with
 * in `key` at save time.
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
	/** UI-only: true once the user has manually edited the key. */
	keyTouched?: boolean;
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

/**
 * Keys reserved at the UI level because they would shadow top-level item
 * JSON fields and cause confusion in API responses / rendering.
 * The backend does not explicitly reject these, but using them as schema
 * field keys is strongly discouraged.
 */
export const RESERVED_FIELD_KEYS: ReadonlySet<string> = new Set([
	'id',
	'slug',
	'ref',
	'title',
	'content',
	'tags',
	'pinned',
	'sort_order',
	'parent_id',
	'created_by',
	'last_modified_by',
	'source',
	'created_at',
	'updated_at',
	'deleted_at',
	'fields',
	'item_number',
	'workspace_id',
	'collection_id'
]);

/** Maximum length of a slugified key. */
export const MAX_FIELD_KEY_LENGTH = 40;

/**
 * Convert a free-text label into a safe field key.
 *
 * Rules:
 * - lowercase
 * - strip leading/trailing whitespace
 * - strip any character that isn't [a-z0-9_\s-]
 * - collapse any run of whitespace or hyphens to a single underscore
 * - collapse consecutive underscores
 * - trim leading/trailing underscores
 * - truncate to MAX_FIELD_KEY_LENGTH
 */
export function slugifyKey(input: string): string {
	return input
		.toLowerCase()
		.trim()
		.replace(/[^a-z0-9_\s-]/g, '')
		.replace(/[\s-]+/g, '_')
		.replace(/_+/g, '_')
		.replace(/^_+|_+$/g, '')
		.slice(0, MAX_FIELD_KEY_LENGTH);
}

/**
 * Validate a field key for structural correctness.
 * Does NOT check for duplicates — the caller must do that since it
 * requires context of sibling fields.
 *
 * @returns null if valid, an error message string otherwise
 */
export function validateFieldKey(key: string): string | null {
	const trimmed = key.trim();
	if (!trimmed) return 'Key is required';
	if (RESERVED_FIELD_KEYS.has(trimmed.toLowerCase())) {
		return `"${trimmed}" is a reserved key`;
	}
	if (!/^[a-z][a-z0-9_]*$/.test(trimmed)) {
		return 'Key must start with a letter and contain only lowercase letters, digits, and underscores';
	}
	if (trimmed.length > MAX_FIELD_KEY_LENGTH) {
		return `Key must be ${MAX_FIELD_KEY_LENGTH} characters or fewer`;
	}
	return null;
}

/**
 * Does a field of this type support a default value?
 *
 * Shared by FieldEditor (to decide whether to render the default input)
 * and by both modals' save paths (to gate `default` emission so stale
 * values from a prior type don't leak into the saved schema when the
 * user switches types).
 *
 * multi_select: deliberately excluded — array defaults are complex and
 * deferred.
 * relation: excluded — a default target item isn't meaningful here.
 */
export function typeSupportsDefault(type: FieldDef['type']): boolean {
	return (
		type === 'text' ||
		type === 'url' ||
		type === 'number' ||
		type === 'date' ||
		type === 'checkbox' ||
		type === 'select'
	);
}

/** Create an empty EditableField for a new (unsaved) field. */
export function blankField(): EditableField {
	return {
		key: '',
		label: '',
		type: 'text',
		options: [],
		originalOptions: [],
		terminalOptions: [],
		keyTouched: false
	};
}

/**
 * Convert a FieldDef from a template or a saved collection into an
 * EditableField. Used by both modals when hydrating field state.
 *
 * @param def the source FieldDef
 * @param existing true if this is a saved field being edited (freezes the key);
 *                 false if it's being introduced (e.g. from a template) and
 *                 should still have its key auto-synced until touched.
 *                 Templates pass true so their keys aren't overwritten.
 */
export function fieldFromDef(def: FieldDef, existing: boolean): EditableField {
	return {
		key: def.key,
		label: def.label || def.key,
		type: def.type,
		options: def.options ? [...def.options] : [],
		originalOptions: existing && def.options ? [...def.options] : [],
		terminalOptions: def.terminal_options ? [...def.terminal_options] : [],
		required: def.required,
		computed: def.computed,
		collection: def.collection,
		suffix: def.suffix,
		default: def.default,
		// Both existing and template fields start with keyTouched=true so
		// the key is preserved verbatim, not overwritten by slugify(label).
		keyTouched: true
	};
}
