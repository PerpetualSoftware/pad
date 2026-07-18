import { describe, it, expect } from 'vitest';
import type { Item, PaneTarget } from '$lib/types';
import { resolvePaneTarget, isSamePaneTarget } from './paneTarget';

// Minimal Item stand-in — resolution/guard only touch id/slug/item_number/
// collection_prefix (the fields `formatItemRef`/`itemUrlId` read).
function item(opts: {
	id: string;
	slug: string;
	item_number?: number;
	collection_prefix?: string;
}): Item {
	return {
		id: opts.id,
		slug: opts.slug,
		item_number: opts.item_number,
		collection_prefix: opts.collection_prefix,
	} as unknown as Item;
}

const task5 = item({ id: 'id-1', slug: 'task-5-slug', item_number: 5, collection_prefix: 'TASK' });
const noRefItem = item({ id: 'id-2', slug: 'legacy-item' }); // no item_number → formatItemRef is null

describe('resolvePaneTarget — candidate extraction (no current item)', () => {
	it('prefers ref over slug over href', () => {
		expect(resolvePaneTarget({ ref: 'TASK-9', slug: 'other-slug', href: '/a/b/c/other' })).toBe(
			'TASK-9',
		);
	});

	it('falls back to slug when no ref', () => {
		expect(resolvePaneTarget({ slug: 'my-slug', href: '/a/b/c/other' })).toBe('my-slug');
	});

	it('falls back to an href trailing segment when no ref/slug', () => {
		expect(resolvePaneTarget({ href: '/alice/myws/tasks/TASK-9' })).toBe('TASK-9');
	});

	it('strips query/hash and trailing slash from an href', () => {
		expect(resolvePaneTarget({ href: '/alice/myws/tasks/TASK-9?foo=1' })).toBe('TASK-9');
		expect(resolvePaneTarget({ href: '/alice/myws/tasks/TASK-9#section' })).toBe('TASK-9');
		expect(resolvePaneTarget({ href: '/alice/myws/tasks/TASK-9/' })).toBe('TASK-9');
	});

	it('returns null when the target carries nothing resolvable', () => {
		expect(resolvePaneTarget({})).toBeNull();
		expect(resolvePaneTarget({ collectionSlug: 'tasks' })).toBeNull();
		expect(resolvePaneTarget({ href: '' })).toBeNull();
		expect(resolvePaneTarget({ href: '/' })).toBeNull();
	});
});

describe('isSamePaneTarget — same-item guard', () => {
	it('is false with no current item', () => {
		expect(isSamePaneTarget({ ref: 'TASK-5' }, null)).toBe(false);
		expect(isSamePaneTarget({ ref: 'TASK-5' }, undefined)).toBe(false);
	});

	it('matches by ref', () => {
		expect(isSamePaneTarget({ ref: 'TASK-5' }, task5)).toBe(true);
	});

	it('SLUG-vs-REF alias: a target naming the item by slug matches even though the item is shown by its ref', () => {
		// The classic alias case TASK-2158 guards against: the pane is showing
		// task5 (which itemUrlId would open under its REF, "TASK-5"), but a
		// link surface names the same item by its SLUG instead.
		expect(isSamePaneTarget({ slug: task5.slug }, task5)).toBe(true);
	});

	it('an item with no ref (item_number unset) only matches by id/slug', () => {
		expect(isSamePaneTarget({ ref: 'TASK-5' }, noRefItem)).toBe(false);
		expect(isSamePaneTarget({ slug: 'legacy-item' }, noRefItem)).toBe(true);
	});

	it('matches by id when the target resolves an id-shaped candidate', () => {
		expect(isSamePaneTarget({ ref: task5.id }, task5)).toBe(true);
	});

	it('matches via an href whose trailing segment is the ref or slug', () => {
		expect(isSamePaneTarget({ href: '/alice/myws/tasks/TASK-5' }, task5)).toBe(true);
		expect(isSamePaneTarget({ href: '/alice/myws/tasks/task-5-slug' }, task5)).toBe(true);
	});

	it('is false for a genuinely different item', () => {
		const other = item({ id: 'id-3', slug: 'other-slug', item_number: 9, collection_prefix: 'TASK' });
		expect(isSamePaneTarget({ ref: 'TASK-5' }, other)).toBe(false);
		expect(isSamePaneTarget({ slug: 'task-5-slug' }, other)).toBe(false);
	});

	it('is false when the target resolves to nothing', () => {
		expect(isSamePaneTarget({}, task5)).toBe(false);
	});
});

describe('resolvePaneTarget — same-item guard short-circuits to null', () => {
	it('a slug-alias of the current item resolves to null (no-op), not a ref/slug string', () => {
		// target names task5 by slug; task5 is showing under its ref form.
		// Returning task5's own ref here (rather than null) could itself
		// mismatch the pane's actual open `?item=` value if that happens to be
		// slug-shaped (e.g. a shared/deep-linked URL) — see paneTarget.ts.
		const target: PaneTarget = { slug: 'task-5-slug' };
		expect(resolvePaneTarget(target, task5)).toBeNull();
	});

	it('a ref-alias of the current item resolves to null (no-op)', () => {
		const target: PaneTarget = { ref: 'TASK-5' };
		expect(resolvePaneTarget(target, task5)).toBeNull();
	});

	it('an id-alias of the current item resolves to null (no-op)', () => {
		const target: PaneTarget = { ref: task5.id };
		expect(resolvePaneTarget(target, task5)).toBeNull();
	});

	it('falls back to the raw candidate for a different item even with a current item present', () => {
		const target: PaneTarget = { ref: 'TASK-9' };
		expect(resolvePaneTarget(target, task5)).toBe('TASK-9');
	});

	it('resolves normally (no guard) when current has no matching id/ref/slug', () => {
		const target: PaneTarget = { slug: 'legacy-item' };
		expect(resolvePaneTarget(target, task5)).toBe('legacy-item');
	});
});
