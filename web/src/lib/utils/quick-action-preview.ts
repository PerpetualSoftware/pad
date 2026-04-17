import type { Collection, Item } from '$lib/types';
import { formatItemRef, parseFields } from '$lib/types';

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
export function contextFromItem(item: Item, collection: Collection): PreviewContext {
	const fields = parseFields(item);
	return {
		ref: formatItemRef(item) ?? '',
		title: item.title ?? '',
		status: String(fields['status'] ?? ''),
		priority: String(fields['priority'] ?? ''),
		collection: collection.name,
		content: item.content ? item.content.slice(0, 200) : '',
		fields: Object.entries(fields)
			.map(([k, v]) => `${k}: ${v}`)
			.join(', '),
		plan: String(fields['plan'] ?? ''),
		phase: String(fields['phase'] ?? fields['plan'] ?? '')
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
