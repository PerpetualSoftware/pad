import { afterEach, describe, expect, it } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection } from '$lib/types';
import EmptyState from './EmptyState.svelte';

// BUG-3054: the empty-state copy is looked up by collection slug, which a user
// picks. A collection named "Constructor" (slug `constructor`) rendered Object's
// constructor function as its message, since `messages['constructor'] ?? …`
// found an inherited, truthy member.
afterEach(() => cleanup());

function coll(slug: string, name: string): Collection {
	return { slug, name, icon: '', id: 'c1' } as unknown as Collection;
}

describe('EmptyState for a slug that names an Object.prototype member (BUG-3054)', () => {
	for (const [slug, name] of [
		['constructor', 'Constructor'],
		['tostring', 'ToString'],
		['__proto__', 'Proto'],
	] as const) {
		it(`shows the generic copy for ${slug}, not an inherited member`, () => {
			const { container } = render(EmptyState, { props: { collection: coll(slug, name), wsSlug: 'ws' } });
			const text = container.textContent ?? '';
			expect(text).toContain(`No ${name.toLowerCase()} yet.`);
			expect(text).not.toMatch(/function|native code|\[object Object\]/);
		});
	}

	it('CONTROL: a slug with its own copy still gets it', () => {
		const { container } = render(EmptyState, { props: { collection: coll('tasks', 'Tasks'), wsSlug: 'ws' } });
		expect(container.textContent).toContain('No tasks yet. Create your first task');
	});
});
