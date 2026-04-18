// Shared helpers for grouping workspace templates by category in pickers.
// Mirrors internal/collections/templates.go (CategoryOrder + CategoryLabel)
// so the CLI and web UI use the same canonical ordering and labels.

import type { WorkspaceTemplate } from '$lib/types';

export const CATEGORY_ORDER = [
	'software',
	'people',
	'research',
	'content',
	'operations',
	'personal',
] as const;

const CATEGORY_LABELS: Record<string, string> = {
	software: 'Software',
	people: 'People',
	research: 'Research',
	content: 'Content',
	operations: 'Operations',
	personal: 'Personal',
};

export function categoryLabel(slug: string | undefined): string {
	if (!slug) return 'Other';
	return CATEGORY_LABELS[slug] ?? slug;
}

export interface TemplateGroup {
	category: string;
	label: string;
	templates: WorkspaceTemplate[];
}

// groupTemplatesByCategory returns templates bucketed by category in
// CATEGORY_ORDER. Any template with a category not in CATEGORY_ORDER
// (including undefined) is collected into a trailing "other" group so
// nothing is hidden from the picker.
export function groupTemplatesByCategory(templates: WorkspaceTemplate[]): TemplateGroup[] {
	const byCat = new Map<string, WorkspaceTemplate[]>();
	for (const t of templates) {
		const cat = t.category ?? '';
		const bucket = byCat.get(cat);
		if (bucket) bucket.push(t);
		else byCat.set(cat, [t]);
	}

	const groups: TemplateGroup[] = [];
	for (const cat of CATEGORY_ORDER) {
		const items = byCat.get(cat);
		if (items && items.length > 0) {
			groups.push({ category: cat, label: categoryLabel(cat), templates: items });
			byCat.delete(cat);
		}
	}
	// Append any leftover categories (custom or empty) in insertion order.
	for (const [cat, items] of byCat) {
		if (items.length > 0) {
			groups.push({ category: cat, label: categoryLabel(cat), templates: items });
		}
	}
	return groups;
}
