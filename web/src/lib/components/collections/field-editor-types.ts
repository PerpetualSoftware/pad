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
 * - `originalType` snapshots the field's type at load time. The
 *   Edit-modal save path uses this to decide whether a default on an
 *   otherwise-unsupported-for-UI type (multi_select, relation) is
 *   opaque imported state to preserve or stale state from an in-session
 *   type switch that should be dropped.
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
	/** UI-only: the field's type at load time, used to detect in-session type switches. */
	originalType?: FieldDef['type'];
	/**
	 * UI-only: the field's default at load time. Paired with `originalType`
	 * by the Edit-modal save path to detect when a default on a UI-
	 * unsupported type has been altered in-session (e.g. via a
	 * supported-type detour) and should be dropped rather than preserved.
	 */
	originalDefault?: unknown;
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

/**
 * Coerce a raw `field.default` value to match the active field type,
 * returning undefined when the value isn't representable (so the caller
 * can drop it).
 *
 * This guards against two failure modes that the save paths would
 * otherwise propagate into the saved schema:
 *
 * 1. Type-switch drift: a user sets a default on a `checkbox` field
 *    (boolean `true`), switches the type to `text`, and the stale
 *    boolean would be serialized as the text default. ValidateFields
 *    would then auto-apply it to new items without re-validating.
 *
 * 2. Select whitespace drift: option text is normalized via `o.trim()`
 *    at serialize time (e.g. "open " -> "open"), but the raw default
 *    value captured at pick time still carries the whitespace. The
 *    saved schema would have `options:["open"]` + `default:"open "` —
 *    a default that isn't in the allowed set.
 *
 * @param raw the current `field.default` value (polymorphic)
 * @param type the active field type at save time
 * @param options for `select` fields, the already-normalized option set.
 *   When supplied, the coerced default must be one of these values or
 *   it is dropped.
 */
export function coerceDefault(
	raw: unknown,
	type: FieldDef['type'],
	options?: string[]
): unknown {
	if (raw === undefined || raw === null) return undefined;
	switch (type) {
		case 'text':
		case 'url':
			return typeof raw === 'string' && raw !== '' ? raw : undefined;
		case 'number': {
			if (typeof raw === 'number' && Number.isFinite(raw)) return raw;
			if (typeof raw === 'string' && raw.trim() !== '') {
				const n = Number(raw);
				return Number.isFinite(n) ? n : undefined;
			}
			return undefined;
		}
		case 'date': {
			// Require a real calendar date in ISO 8601 form. HTML <input
			// type="date"> emits YYYY-MM-DD; imported schemas may carry
			// RFC3339 datetime strings. A regex alone isn't enough — it would
			// accept impossible dates like "2026-99-99" or "2026-01-32",
			// which then survive into the saved schema and get injected into
			// new items by ValidateFields without re-checking.
			if (typeof raw !== 'string') return undefined;
			const trimmed = raw.trim();

			// Plain date: YYYY-MM-DD. Round-trip verify against Date to
			// reject out-of-range months/days (e.g. 2026-01-32 would be
			// silently rolled to 2026-02-01 otherwise).
			const ymd = /^(\d{4})-(\d{2})-(\d{2})$/.exec(trimmed);
			if (ymd) {
				const year = Number(ymd[1]);
				const month = Number(ymd[2]);
				const day = Number(ymd[3]);
				if (month < 1 || month > 12 || day < 1 || day > 31) return undefined;
				const parsed = new Date(Date.UTC(year, month - 1, day));
				if (
					parsed.getUTCFullYear() === year &&
					parsed.getUTCMonth() + 1 === month &&
					parsed.getUTCDate() === day
				) {
					return trimmed;
				}
				return undefined;
			}

			// Strict RFC3339 datetime: YYYY-MM-DDThh:mm:ss[.frac](Z|±hh:mm).
			// Timezone is required. Offsets must include the colon
			// separator (+01:00, not +0100). Seconds are required to match
			// RFC3339 (the backend's time.RFC3339 parser also requires
			// them). `new Date(...)` alone is too permissive — it silently
			// rolls invalid dates (e.g. "2026-02-31T10:00:00Z" becomes
			// March 3), so component-level round-trip verification is
			// still needed.
			const dt =
				/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})$/.exec(
					trimmed
				);
			if (dt) {
				const year = Number(dt[1]);
				const month = Number(dt[2]);
				const day = Number(dt[3]);
				const hour = Number(dt[4]);
				const min = Number(dt[5]);
				const sec = Number(dt[6]);
				const tz = dt[7];
				if (month < 1 || month > 12 || day < 1 || day > 31) return undefined;
				if (hour > 23 || min > 59 || sec > 59) return undefined;
				// Validate the timezone offset numerically. The regex only
				// enforces the `±hh:mm` shape; offsets like `+99:99` would
				// otherwise slip through. Go's time.RFC3339 (backend
				// parser) accepts any offset within ±23:59 minutes.
				if (tz !== 'Z') {
					const offH = Number(tz.slice(1, 3));
					const offM = Number(tz.slice(4, 6));
					if (offH > 23 || offM > 59) return undefined;
				}
				// Round-trip verify the date components: if the day is out
				// of range for the month (e.g. Feb 31), Date.UTC rolls it
				// forward and the components won't match.
				const parsed = new Date(Date.UTC(year, month - 1, day));
				if (
					parsed.getUTCFullYear() === year &&
					parsed.getUTCMonth() + 1 === month &&
					parsed.getUTCDate() === day
				) {
					return trimmed;
				}
			}
			return undefined;
		}
		case 'checkbox':
			return typeof raw === 'boolean' ? raw : undefined;
		case 'select': {
			if (typeof raw !== 'string') return undefined;
			const trimmed = raw.trim();
			if (trimmed === '') return undefined;
			// When the caller supplies the normalized options set, the
			// default must be one of them. This catches (a) whitespace
			// drift between option text and default text, and (b) stale
			// defaults from when the options list was different.
			if (options && !options.includes(trimmed)) return undefined;
			return trimmed;
		}
		default:
			return undefined;
	}
}

/**
 * Deep-equality check for default values, tolerant of the polymorphic
 * shapes a default can take (string | number | boolean | string[] for
 * multi_select). Uses JSON stringification — fine for schema defaults,
 * which are always JSON-serializable primitives and arrays.
 *
 * Used by the Edit modal to decide whether a default on a UI-
 * unsupported-for-editing type has been mutated in-session.
 */
export function defaultsEqual(a: unknown, b: unknown): boolean {
	if (a === b) return true;
	if (a === undefined || b === undefined) return false;
	try {
		return JSON.stringify(a) === JSON.stringify(b);
	} catch {
		return false;
	}
}

/**
 * Matches the backend's `safeDoneFieldKey` regex in
 * `internal/models/terminal.go`. A done-field candidate must be an
 * identifier-shaped string the backend will accept, or it falls back to
 * the literal "status" there — the UI needs to mirror that or it can
 * falsely light up an "Active" pill on a field the server has actually
 * rejected (e.g. legacy schemas with keys like `foo.bar` or
 * `resolution-v2`).
 */
const SAFE_DONE_FIELD_KEY = /^[a-zA-Z][a-zA-Z0-9_]*$/;

/**
 * Returns true if the given key would be accepted by the backend's
 * DoneFieldKey resolver. Used by the collection modals' `activeDoneField`
 * derivations to keep UI state in sync with persisted behavior.
 */
export function isSafeDoneFieldKey(key: string): boolean {
	return SAFE_DONE_FIELD_KEY.test(key);
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
		originalType: def.type,
		originalDefault: def.default,
		// Both existing and template fields start with keyTouched=true so
		// the key is preserved verbatim, not overwritten by slugify(label).
		keyTouched: true
	};
}
