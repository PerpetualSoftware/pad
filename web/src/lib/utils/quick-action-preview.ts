import type { Collection, Item, ItemIndexRow } from '$lib/types';
import { safeString, safeText } from '$lib/fields/fieldShape';
import { formatItemRef, parseFields } from '$lib/types';
import { fieldDefFor } from '$lib/collections/categoricalFieldValue';
import { isRelationType, relationValuesOf } from '$lib/items/relationFieldTypes';

/**
 * The set of template variables the QuickActionsMenu substitutes at
 * runtime. Keep this list in lockstep with the runtime resolver in
 * `$lib/components/common/QuickActionsMenu.svelte` so the preview shows
 * exactly what users will get when they actually invoke the action.
 */
export const TEMPLATE_VARIABLES = [
	'ref',
	'title',
	'status',
	'priority',
	'collection',
	'content',
	'fields',
	'plan',
	'phase'
] as const;

export type TemplateVariable = (typeof TEMPLATE_VARIABLES)[number];

const TEMPLATE_VARIABLE_SET: ReadonlySet<string> = new Set(TEMPLATE_VARIABLES);

export type PreviewContext = Record<TemplateVariable, string>;

/**
 * THE ONE SUBSTITUTION for a categorical template variable (BUG-3067, lead
 * ruling on the trail).
 *
 * A `status` or `priority` retyped to a relation stores an item id, and both
 * this module and `QuickActionsMenu` pasted that id straight into a prompt the
 * user then hands to an agent. The chips answer this by WITHHOLDING; a prompt
 * variable cannot — `{status}` has to become something, and a blank is a lie of
 * a different kind.
 *
 * So the ruling, matching what #1352 did for chips: resolve to the target's
 * TITLE, fall back to its REF, and never emit the raw id. A list joins with ', '.
 *
 * THE FALLBACK IS FOR A ROW WITH NO TITLE, not for a row that does not resolve,
 * and an earlier version of this comment said the latter — which is not a thing
 * this function can do. A ref is built FROM the row (its collection prefix and
 * item number); an id the index cannot resolve yields no row and therefore no
 * ref, so there is nothing to fall back to. Such a value yields the empty string
 * because nothing true is left to say about it, which is the one case the
 * ruling's "never empty by choice" does not reach.
 *
 * It lives HERE, and `QuickActionsMenu` imports it, because the two were one
 * site with two implementations: this module's own doc says it MIRRORS the
 * menu so the preview shows what copying produces, and two mirrors drift.
 */
export function categoricalTemplateValue(
	collection: Collection | undefined,
	key: string,
	raw: unknown,
	resolve: (id: string) => ItemIndexRow | null | undefined,
): string {
	const field = collection ? fieldDefFor([collection], collection.slug, key) : undefined;
	if (!field || !isRelationType(field.type)) {
		return safeText(raw);
	}
	const ids = relationValuesOf(field.type, raw);
	const labels: string[] = [];
	for (const id of ids) {
		const row = resolve(id);
		if (!row) continue;
		labels.push(row.title || formatItemRef(row) || '');
	}
	return labels.filter(Boolean).join(', ');
}


/**
 * Reshape an item-scope preview context into a collection-scope one by
 * clearing all item-only variables. Mirrors the runtime in
 * QuickActionsMenu.resolvePrompt where `item` is unset for collection-
 * scope actions: `ref`, `title`, `status`, `priority`, `content`, `fields`,
 * `plan`, `phase` all resolve to empty strings. Only `{collection}`
 * survives.
 *
 * Used so the Quick Actions preview renders the same output the user
 * will actually get when they click a collection action.
 */
export function toCollectionScope(ctx: PreviewContext): PreviewContext {
	return {
		ref: '',
		title: '',
		status: '',
		priority: '',
		collection: ctx.collection,
		content: '',
		fields: '',
		plan: '',
		phase: ''
	};
}

/**
 * Placeholder context used when no real item is available — e.g. in the
 * Create modal (collection doesn't exist yet) or when the collection is
 * empty.
 */
export function placeholderContext(collectionName: string): PreviewContext {
	return {
		ref: 'TASK-42',
		title: 'Example item title',
		status: 'open',
		priority: 'medium',
		collection: collectionName || 'Your collection',
		content: '(item content goes here)',
		fields: 'status: open, priority: medium',
		plan: '',
		phase: ''
	};
}

/**
 * Build a preview context from a real Item + Collection pair. Mirrors the
 * substitution logic in QuickActionsMenu.svelte so the preview is a true
 * representation of what copying the prompt would produce.
 */
export function contextFromItem(
	item: Item,
	collection: Collection,
	resolve: (id: string) => ItemIndexRow | null | undefined = () => null,
): PreviewContext {
	const fields = parseFields(item);
	return {
		ref: formatItemRef(item) ?? '',
		title: item.title ?? '',
		status: categoricalTemplateValue(collection, 'status', fields['status'], resolve),
		priority: categoricalTemplateValue(collection, 'priority', fields['priority'], resolve),
		collection: collection.name,
		content: item.content ? item.content.slice(0, 200) : '',
		// safeString / safeText, not `${v}` / String (BUG-3052): a stored
		// `{"toString":0}` threw here. Same text for every value String handled.
		fields: Object.entries(fields)
			.map(([k, v]) => `${k}: ${safeString(v)}`)
			.join(', '),
		plan: safeText(fields['plan']),
		phase: safeText(fields['phase'] ?? fields['plan'])
	};
}

/**
 * A single segment of a parsed prompt.
 *
 * - `text`: literal text between variable references.
 * - `known`: a `{var}` that matches a known template variable. Includes
 *   the resolved value from the preview context so the preview can
 *   render exactly what the user would get.
 * - `unknown`: a `{var}` whose name is NOT in the known set — likely a
 *   user typo. Rendered in red so the error is visible.
 */
export type PromptSegment =
	| { type: 'text'; value: string }
	| { type: 'known'; name: TemplateVariable; resolved: string }
	| { type: 'unknown'; name: string };

// A variable reference starts with a letter or underscore and continues
// with letters / digits / underscores. Kept intentionally narrow so we
// don't accidentally treat JSON snippets or arbitrary braces as vars.
const VAR_PATTERN = /\{([a-zA-Z_][a-zA-Z0-9_]*)\}/g;

/**
 * Tokenize a prompt into segments using the supplied context. Unknown
 * variable names (typos, unsupported vars) are emitted as `unknown`
 * segments so the UI can flag them.
 */
export function parsePrompt(prompt: string, ctx: PreviewContext): PromptSegment[] {
	const segments: PromptSegment[] = [];
	let lastIndex = 0;
	// Reset regex lastIndex so repeated calls behave correctly.
	VAR_PATTERN.lastIndex = 0;
	let match: RegExpExecArray | null;
	while ((match = VAR_PATTERN.exec(prompt)) !== null) {
		if (match.index > lastIndex) {
			segments.push({ type: 'text', value: prompt.slice(lastIndex, match.index) });
		}
		const name = match[1];
		if (TEMPLATE_VARIABLE_SET.has(name)) {
			segments.push({
				type: 'known',
				name: name as TemplateVariable,
				resolved: ctx[name as TemplateVariable]
			});
		} else {
			segments.push({ type: 'unknown', name });
		}
		lastIndex = VAR_PATTERN.lastIndex;
	}
	if (lastIndex < prompt.length) {
		segments.push({ type: 'text', value: prompt.slice(lastIndex) });
	}
	return segments;
}
