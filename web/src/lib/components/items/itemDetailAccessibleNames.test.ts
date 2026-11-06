import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';

const markup = readFileSync(new URL('./ItemDetail.svelte', import.meta.url), 'utf8')
	.replace(/<!--[\s\S]*?-->/g, '');

describe('ItemDetail accessible names (BUG-4336)', () => {
	it('names the title textarea', () => {
		const title = markup.match(/<textarea\b[^>]*class="title-input"[\s\S]*?<\/textarea>/)?.[0];
		expect(title).toBeDefined();
		expect(title).toContain('aria-label="Item title"');
	});

	it('passes the visible property label to FieldEditor', () => {
		const field = markup.match(/<FieldEditor\b[\s\S]*?\/>/)?.[0];
		expect(field).toBeDefined();
		expect(field).toContain('ariaLabel={field.label}');
	});
});
