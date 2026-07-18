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

	it('refuses a cross-workspace resolver href (/-/r/{workspace}/{ref}) rather than misreading its trailing ref as local', () => {
		// wikiLinksToMarkdown/renderMarkdown emit this shape for [[otherWs::REF]]
		// links. Taking the trailing segment at face value would drill the
		// CURRENT workspace's pane to a same-numbered local item — the wrong
		// item. This link shape must fall through to a normal navigation
		// instead (Codex review).
		expect(resolvePaneTarget({ href: '/-/r/other-workspace/TASK-9' })).toBeNull();
	});

	it('refuses an ABSOLUTE cross-workspace resolver href too', () => {
		// HTMLAnchorElement.href always returns the fully-resolved absolute
		// URL, not the raw attribute — a future click-interceptor reading it
		// off a live DOM anchor must still be caught (Codex review).
		expect(
			resolvePaneTarget({ href: 'http://localhost:5173/-/r/other-workspace/TASK-9' }),
		).toBeNull();
		expect(resolvePaneTarget({ href: 'https://mypad.example.com/-/r/other/TASK-9' })).toBeNull();
	});

	it('distrusts the WHOLE target (including ref/slug) when href signals cross-workspace', () => {
		// A target that carries both a `ref` and a cross-workspace `href`
		// contradicts this type's own contract — the href is unambiguous
		// evidence of non-locality, so it wins rather than risk opening
		// whatever local item `ref` happens to name (Codex review).
		expect(
			resolvePaneTarget({ ref: 'TASK-9', href: '/-/r/other-workspace/TASK-9' }),
		).toBeNull();
		expect(
			resolvePaneTarget({ slug: 'some-slug', href: '/-/r/other-workspace/TASK-9' }),
		).toBeNull();
	});

	it('still resolves a same-workspace ABSOLUTE href normally', () => {
		expect(resolvePaneTarget({ href: 'http://localhost:5173/alice/myws/tasks/TASK-9' })).toBe(
			'TASK-9',
		);
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

	it('matches a ref case-insensitively', () => {
		expect(isSamePaneTarget({ ref: 'task-5' }, task5)).toBe(true);
		expect(isSamePaneTarget({ ref: 'Task-5' }, task5)).toBe(true);
	});

	it('STALE-PREFIX alias: a ref-shaped candidate matches by item NUMBER alone, ignoring a stale prefix from before a collection move', () => {
		// Mirrors the server's GetItemByRef fallback (item numbers are
		// workspace-unique): a wiki-link written as "[[PLAN-5]]" before the
		// item moved to the Tasks collection (now TASK-5, item_number 5) still
		// names the same item — the guard must catch it, not push a redundant
		// re-target (Codex review).
		expect(isSamePaneTarget({ ref: 'PLAN-5' }, task5)).toBe(true);
		expect(isSamePaneTarget({ ref: 'plan-5' }, task5)).toBe(true);
	});

	it('a ref-shaped candidate with the right prefix but wrong number is a different item', () => {
		expect(isSamePaneTarget({ ref: 'TASK-6' }, task5)).toBe(false);
	});

	it('does NOT treat a digit-bearing slug as a ref, even when its trailing number coincides', () => {
		// The server's parseItemRef requires a LETTERS-ONLY prefix (no digits);
		// a slug like "roadmap2-5" is not ref-shaped by that grammar and must
		// stay a plain slug candidate — not misread as ref number 5, which
		// would false-positive against an unrelated item TASK-5 (Codex review
		// — PR diff pass).
		expect(isSamePaneTarget({ slug: 'roadmap2-5' }, task5)).toBe(false);
		expect(isSamePaneTarget({ href: '/alice/myws/docs/roadmap2-5' }, task5)).toBe(false);
	});

	it('a cross-workspace resolver href never false-positives, even when the numbers coincide', () => {
		// A different workspace's TASK-5 is NOT this workspace's task5, even
		// though the trailing ref segment is identical.
		expect(isSamePaneTarget({ href: '/-/r/other-workspace/TASK-5' }, task5)).toBe(false);
	});

	it('a ref-sourced candidate is judged ONLY as a ref, never falling through to a raw slug compare', () => {
		// The exact collision Codex's PR-diff pass flagged: current is
		// slugged "plan-6" but numbered 5; a target explicitly carrying
		// `{ ref: "plan-6" }` names a DIFFERENT item (number 6) and must NOT
		// be swallowed as a same-item alias just because the ref string
		// happens to equal current's slug — that would silently drop a
		// legitimate navigation.
		const current = item({ id: 'id-9', slug: 'plan-6', item_number: 5, collection_prefix: 'TASK' });
		expect(isSamePaneTarget({ ref: 'plan-6' }, current)).toBe(false);
		// A slug-sourced candidate is likewise judged only as a slug: it
		// still matches when it's genuinely current's own slug.
		expect(isSamePaneTarget({ slug: 'plan-6' }, current)).toBe(true);
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
