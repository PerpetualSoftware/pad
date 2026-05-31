// Shared types + helpers for the public read-only collection view renderers
// (TASK-1679 / PLAN-1677).
//
// These renderers consume the share-link payload produced by TASK-1678 (which
// ships in parallel), so EVERYTHING here parses defensively: `settings`,
// `schema`, and per-item `fields` may arrive as a JSON string OR an
// already-parsed object. We never assume; we coerce. TASK-1680 reconciles
// against the final wire shape — until then these helpers are the single
// choke-point for tolerating either form.
//
// Deliberately decoupled from `$lib/types`' `Item`/`Collection` DB row shapes
// and from the interactive BoardView/ListView/TableView (which depend on
// `page.params`, internal links, and mutation handlers — exactly what a
// logged-out, read-only audience must not see). The color/label vocabularies
// mirror the in-app helpers so a shared kanban looks like the owner's kanban.

import type { FieldDef } from '$lib/types';

/** Collection settings as understood by the public renderers. View-type +
 *  grouping/sort knobs only — interactive-only settings are ignored. */
export interface PublicViewSettings {
	default_view: 'list' | 'board' | 'table';
	board_group_by?: string;
	list_sort_by?: string;
	list_group_by?: string;
	layout?: string;
}

/** The collection branch of the share payload. `settings`/`schema` are
 *  parsed defensively from string-or-object by `parsePublicCollection`. */
export interface PublicCollection {
	name: string;
	icon?: string;
	description?: string;
	settings: PublicViewSettings;
	fields: FieldDef[];
}

/** One item in the share payload, normalized. `fields` is parsed from
 *  string-or-object; `content` is the raw markdown body (rendering is the
 *  renderer's concern — kept here so a future inline expand (TASK-1684) has
 *  the body without a re-fetch). `key` is a stable, unique identity assigned
 *  at parse time (the item's position in the payload) — refs may be empty and
 *  titles aren't unique, so renderers key `{#each}` blocks on this instead. */
export interface PublicItem {
	key: string;
	title: string;
	ref: string;
	fields: Record<string, unknown>;
	content: string;
}

// ── Defensive parsing ───────────────────────────────────────────────────────

/** Accept a value that may be a JSON string, an object, or null/undefined and
 *  return a plain record. Never throws. */
export function coerceObject(value: unknown): Record<string, unknown> {
	if (value == null) return {};
	if (typeof value === 'string') {
		const trimmed = value.trim();
		if (!trimmed) return {};
		try {
			const parsed = JSON.parse(trimmed);
			return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
				? (parsed as Record<string, unknown>)
				: {};
		} catch {
			return {};
		}
	}
	if (typeof value === 'object' && !Array.isArray(value)) {
		return value as Record<string, unknown>;
	}
	return {};
}

/** Pull a `fields` array out of a schema that may be a JSON string, an object
 *  with a `fields` array, or already an array of FieldDef. Returns a clean
 *  FieldDef[] (entries missing a `key` are dropped). */
export function coerceFields(schema: unknown): FieldDef[] {
	let candidate: unknown = schema;
	if (typeof schema === 'string') {
		const trimmed = schema.trim();
		if (!trimmed) return [];
		try {
			candidate = JSON.parse(trimmed);
		} catch {
			return [];
		}
	}
	let arr: unknown;
	if (Array.isArray(candidate)) {
		arr = candidate;
	} else if (candidate && typeof candidate === 'object') {
		arr = (candidate as Record<string, unknown>).fields;
	}
	if (!Array.isArray(arr)) return [];
	return arr.filter(
		(f): f is FieldDef => !!f && typeof f === 'object' && typeof (f as FieldDef).key === 'string'
	);
}

const DEFAULT_SETTINGS: PublicViewSettings = { default_view: 'list' };

/** Normalize raw `settings` (string-or-object) into PublicViewSettings,
 *  validating `default_view` against the known set and defaulting to 'list'. */
export function coerceSettings(settings: unknown): PublicViewSettings {
	const obj = coerceObject(settings);
	const view = obj.default_view;
	const default_view: PublicViewSettings['default_view'] =
		view === 'board' || view === 'table' || view === 'list' ? view : 'list';
	return {
		default_view,
		board_group_by: typeof obj.board_group_by === 'string' ? obj.board_group_by : undefined,
		list_sort_by: typeof obj.list_sort_by === 'string' ? obj.list_sort_by : undefined,
		list_group_by: typeof obj.list_group_by === 'string' ? obj.list_group_by : undefined,
		layout: typeof obj.layout === 'string' ? obj.layout : undefined
	};
}

/** Normalize the raw collection branch of the share payload into a
 *  PublicCollection. Tolerates `settings` and `schema` arriving as either a
 *  JSON string or an object (TASK-1678 ships in parallel). */
export function parsePublicCollection(raw: unknown): PublicCollection {
	const obj = coerceObject(raw);
	return {
		name: typeof obj.name === 'string' && obj.name ? obj.name : 'Collection',
		icon: typeof obj.icon === 'string' ? obj.icon : undefined,
		description: typeof obj.description === 'string' ? obj.description : undefined,
		settings: coerceSettings(obj.settings),
		fields: coerceFields(obj.schema)
	};
}

/** Normalize one raw item into a PublicItem. `fields` parsed defensively;
 *  `ref` falls back across `ref` / `item_ref`. `index` is the item's position
 *  in the payload, used to derive a stable unique `key`. */
export function parsePublicItem(raw: unknown, index = 0): PublicItem {
	const obj = coerceObject(raw);
	const ref =
		typeof obj.ref === 'string' && obj.ref
			? obj.ref
			: typeof obj.item_ref === 'string'
				? obj.item_ref
				: '';
	return {
		// Refs can be empty and titles aren't unique, so anchor the key on the
		// payload position; prefix the ref when present for readable debugging.
		key: ref ? `${ref}#${index}` : `idx#${index}`,
		title: typeof obj.title === 'string' && obj.title ? obj.title : 'Untitled',
		ref,
		fields: coerceObject(obj.fields),
		content: typeof obj.content === 'string' ? obj.content : ''
	};
}

export function parsePublicItems(raw: unknown): PublicItem[] {
	if (!Array.isArray(raw)) return [];
	return raw.map((item, i) => parsePublicItem(item, i));
}

// ── Large-collection cap ────────────────────────────────────────────────────

/** Maximum items a public share view renders. A shared collection can hold
 *  thousands of items; rendering them all would blow up an anonymous viewer's
 *  page (and our DOM) for no benefit — the audience is reading a snapshot, not
 *  triaging a backlog. We render the first N (the owner's `sort_order`, the
 *  order the payload arrives in) and surface a visible "showing N of M" banner
 *  so nothing is silently truncated. 200 comfortably covers real shared
 *  collections while bounding the worst case. */
export const PUBLIC_ITEM_CAP = 200;

export interface CappedItems {
	items: PublicItem[];
	/** Total before capping. */
	total: number;
	/** True when `total > PUBLIC_ITEM_CAP` and `items` was trimmed. */
	capped: boolean;
}

/** Trim an item list to PUBLIC_ITEM_CAP, reporting whether it was capped so
 *  the renderer can show the "showing N of M" banner. */
export function capItems(items: PublicItem[]): CappedItems {
	if (items.length <= PUBLIC_ITEM_CAP) {
		return { items, total: items.length, capped: false };
	}
	return { items: items.slice(0, PUBLIC_ITEM_CAP), total: items.length, capped: true };
}

// ── Field lookup ──────────────────────────────────────────────────────────

export function findField(fields: FieldDef[], key: string): FieldDef | undefined {
	return fields.find((f) => f.key === key);
}

/** Fields a table/list should show: drop computed fields (they aren't part of
 *  the shared snapshot's meaningful columns). */
export function visibleFields(fields: FieldDef[]): FieldDef[] {
	return fields.filter((f) => !f.computed);
}

/** Resolve the field key to group a board by: explicit `board_group_by`,
 *  else `status` if the schema has one, else the first select field, else ''. */
export function resolveGroupField(collection: PublicCollection): string {
	const explicit = collection.settings.board_group_by;
	if (explicit && findField(collection.fields, explicit)) return explicit;
	if (findField(collection.fields, 'status')) return 'status';
	const firstSelect = collection.fields.find((f) => f.type === 'select');
	return firstSelect?.key ?? '';
}

// ── Presentation helpers (mirror the in-app vocabularies) ───────────────────

/** Title-case a snake/kebab field key or value for display. */
export function formatLabel(value: string): string {
	return value.replace(/[_-]/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
}

/** Stringify any field value for read-only display: arrays join, objects
 *  JSON-stringify, primitives coerce, null/undefined → ''. */
export function formatFieldValue(value: unknown): string {
	if (value === null || value === undefined) return '';
	if (Array.isArray(value)) return value.map((v) => formatFieldValue(v)).join(', ');
	if (typeof value === 'object') {
		try {
			return JSON.stringify(value);
		} catch {
			return '';
		}
	}
	return String(value);
}

/** Status color — mirrors ItemCard.statusColor so a shared board matches the
 *  owner's palette. Returns a CSS custom-property reference. */
export function statusColor(status: string): string {
	switch (status) {
		case 'open':
			return 'var(--text-secondary)';
		case 'in_progress':
			return 'var(--accent-amber)';
		case 'done':
			return 'var(--accent-green)';
		case 'blocked':
			return 'var(--accent-orange)';
		default:
			return 'var(--text-muted)';
	}
}

/** Schema-driven color for a select-field value. Prefers the literal status
 *  vocabulary (so the canonical open/in_progress/done/blocked palette matches
 *  the owner exactly), then falls back to the schema's `terminal_options`: a
 *  value the owner marked terminal (e.g. `shipped`, `closed`, `archived`)
 *  reads as "done" green even though it isn't literally `done`. Non-terminal,
 *  non-canonical values get the neutral muted tone. This keeps colors faithful
 *  to the owner's view while honoring custom status vocabularies that the bare
 *  literal switch would miss. */
export function fieldValueColor(field: FieldDef | undefined, value: string): string {
	if (!value) return 'var(--text-muted)';
	if (field?.key === 'priority') return priorityColor(value);
	// Canonical status palette first — exact owner match.
	if (value === 'open' || value === 'in_progress' || value === 'done' || value === 'blocked') {
		return statusColor(value);
	}
	// Custom vocabulary: terminal options read as "done".
	if (field?.terminal_options?.includes(value)) return 'var(--accent-green)';
	return 'var(--text-muted)';
}

/** Schema-driven board-column accent class. Literal status values map to the
 *  in-app palette; a custom terminal option falls back to the `done` accent so
 *  a "Shipped"/"Closed" column still reads as a finished lane. */
export function columnAccentClassFor(field: FieldDef | undefined, value: string): string {
	switch (value) {
		case 'in_progress':
			return 'col-in-progress';
		case 'done':
			return 'col-done';
		case 'blocked':
			return 'col-blocked';
	}
	if (value && field?.terminal_options?.includes(value)) return 'col-done';
	return '';
}

/** Priority color — mirrors ItemCard.priorityColor. */
export function priorityColor(priority: string): string {
	switch (priority) {
		case 'critical':
			return 'var(--accent-orange)';
		case 'high':
			return 'var(--accent-amber)';
		case 'medium':
			return 'var(--text-secondary)';
		case 'low':
			return 'var(--text-muted)';
		default:
			return 'var(--text-muted)';
	}
}

/** Group `items` by `groupField` value, in option order with any extra values
 *  appended (sorted), and ungrouped ('') last when present. Mirrors
 *  ListView/BoardView grouping so a shared view preserves the owner's columns. */
export function groupItems(
	items: PublicItem[],
	groupField: string,
	optionOrder: string[]
): { value: string; items: PublicItem[] }[] {
	const buckets = new Map<string, PublicItem[]>();
	for (const opt of optionOrder) buckets.set(opt, []);

	const extras: string[] = [];
	let hasUngrouped = false;
	for (const item of items) {
		const raw = item.fields[groupField];
		const value = typeof raw === 'string' ? raw : raw == null ? '' : String(raw);
		if (!value) {
			hasUngrouped = true;
			if (!buckets.has('')) buckets.set('', []);
			buckets.get('')!.push(item);
			continue;
		}
		if (!buckets.has(value)) {
			buckets.set(value, []);
			extras.push(value);
		}
		buckets.get(value)!.push(item);
	}

	const order = [...optionOrder, ...extras.sort()];
	if (hasUngrouped) order.push('');
	// Dedupe while preserving order (an option could also appear in extras edge cases).
	const seen = new Set<string>();
	const result: { value: string; items: PublicItem[] }[] = [];
	for (const v of order) {
		if (seen.has(v)) continue;
		seen.add(v);
		result.push({ value: v, items: buckets.get(v) ?? [] });
	}
	return result;
}
