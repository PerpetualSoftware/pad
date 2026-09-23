import type { ItemConventionMetadata, ItemCreate } from '$lib/types';

/**
 * The create request the Conventions page sends for a new convention.
 *
 * The convention metadata travels as the typed `convention` member, never as a
 * `convention` key inside `fields`: create's `fields` refuses every reserved
 * metadata key (BUG-3163). The sibling keys below are ordinary schema fields.
 */
export function conventionCreatePayload(
	title: string,
	content: string,
	convention: ItemConventionMetadata
): ItemCreate {
	return {
		title,
		content,
		fields: JSON.stringify({
			status: 'active',
			category: convention.category ?? '',
			trigger: convention.trigger ?? 'always',
			scope: convention.surfaces?.[0] ?? 'all',
			priority: convention.enforcement ?? 'should',
			enforcement: convention.enforcement ?? 'should',
			surfaces: convention.surfaces ?? ['all'],
			commands: convention.commands ?? []
		}),
		convention
	};
}
