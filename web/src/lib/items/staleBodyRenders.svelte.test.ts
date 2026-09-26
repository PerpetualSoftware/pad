import { describe, it, expect, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Backlink, Version } from '$lib/types';

// BUG-3050 U3: a body the server marked as behind the live document
// (`content_state: applied_pending_flush`, BUG-3000) says so wherever it is
// rendered. Two treatments, ruled on the trail: a one-line notice above a body
// that is the subject of the view, and a muted dot on a MARKED row where the
// body is one snippet among many. An unmarked body renders nothing new.
//
// Every leg mounts the real component with a marked AND an unmarked input, so
// a render that ignores the marker and a render that marks everything both
// fail. The page bindings (share pages, the item pane, the lists, the palette)
// are covered in e2e/bug-3050-stale-body-renders.spec.ts.

const { backlinkRows } = vi.hoisted(() => ({ backlinkRows: { value: [] as unknown[] } }));

vi.mock('$lib/api/client', () => ({
	api: {
		items: { backlinks: vi.fn(async () => backlinkRows.value) },
		versions: { restore: vi.fn(), get: vi.fn(async () => ({ content: '' })) },
	},
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { identityFence: () => () => true, user: null },
}));

import { isBodyStale, STALE_BODY_NOTICE } from './staleBody';
import { parsePublicItem } from '$lib/components/share/shareView';
import PublicItemExpansion from '$lib/components/share/PublicItemExpansion.svelte';
import TimelineVersionCard from '$lib/components/timeline/TimelineVersionCard.svelte';
import BacklinksPanel from '$lib/components/BacklinksPanel.svelte';

const MARK = 'applied_pending_flush';
const mounted: { root: HTMLElement; instance: ReturnType<typeof mount> }[] = [];

function render<P extends Record<string, unknown>>(component: unknown, props: P): HTMLElement {
	const root = document.body.appendChild(document.createElement('div'));
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const instance = mount(component as any, { target: root, props });
	flushSync();
	mounted.push({ root, instance });
	return root;
}

afterEach(() => {
	for (const m of mounted.splice(0)) {
		unmount(m.instance);
		m.root.remove();
	}
});

describe('isBodyStale', () => {
	it('is true only for the server marker', () => {
		expect(isBodyStale({ content_state: MARK })).toBe(true);
		expect(isBodyStale({ content_state: '' })).toBe(false);
		expect(isBodyStale({ content_state: 'something_else' })).toBe(false);
		expect(isBodyStale({})).toBe(false);
		expect(isBodyStale(null)).toBe(false);
		expect(isBodyStale(undefined)).toBe(false);
	});
});

describe('share: parsePublicItem carries the marker (C2)', () => {
	it('copies content_state into contentStale', () => {
		expect(parsePublicItem({ title: 't', content: 'b', content_state: MARK }).contentStale).toBe(true);
		expect(parsePublicItem({ title: 't', content: 'b' }).contentStale).toBe(false);
	});
});

describe('share: the inline expansion shows the notice on a marked body (C3)', () => {
	function expansion(stale: boolean) {
		const item = parsePublicItem({ title: 't', content: 'body', ...(stale ? { content_state: MARK } : {}) });
		return render(PublicItemExpansion, { item, fields: [], html: '<p>body</p>' });
	}
	it('marked: the notice precedes the body', () => {
		const root = expansion(true);
		const notice = root.querySelector('[data-testid="stale-body-notice"]');
		expect(notice?.textContent).toBe(STALE_BODY_NOTICE);
		const body = root.querySelector('.expansion-content')!;
		expect(notice!.compareDocumentPosition(body) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});
	it('unmarked: no notice', () => {
		expect(expansion(false).querySelector('[data-testid="stale-body-notice"]')).toBeNull();
	});
});

describe('timeline: the diff\'s current side says it may be behind (C5)', () => {
	const version = {
		id: 'v1',
		item_id: 'i1',
		content: 'old',
		is_diff: false,
		change_summary: 'edited',
		created_by: 'user',
		source: 'web',
		created_at: '2026-07-20T00:00:00Z',
	} as unknown as Version;
	function card(currentContentStale: boolean) {
		const root = render(TimelineVersionCard, { version, wsSlug: 'ws', itemSlug: 'ITEM-1', currentContent: 'now', currentContentStale });
		(root.querySelector('.card-header') as HTMLButtonElement).click();
		flushSync();
		return root;
	}
	it('marked: the notice is shown with the diff', () => {
		const root = card(true);
		expect(root.querySelector('[data-testid="stale-body-notice"]')?.textContent).toBe(STALE_BODY_NOTICE);
	});
	it('unmarked: no notice', () => {
		const root = card(false);
		expect(root.querySelector('.diff-container')).not.toBeNull();
		expect(root.querySelector('[data-testid="stale-body-notice"]')).toBeNull();
	});
});

describe('backlinks: a dot on MARKED rows only (C8)', () => {
	function row(id: string, title: string, stale: boolean): Backlink {
		return {
			source_item_id: id,
			source_ref: `DOC-${id}`,
			source_title: title,
			source_slug: `s-${id}`,
			source_collection_slug: 'docs',
			source_collection_name: 'Docs',
			source_collection_icon: '',
			snippet: `...links to it from ${title}...`,
			updated_at: '2026-09-26T00:00:00Z',
			...(stale ? { content_state: MARK } : {}),
		} as unknown as Backlink;
	}
	it('marks the marked row and leaves its neighbour alone', async () => {
		backlinkRows.value = [row('1', 'Marked', true), row('2', 'Plain', false)];
		const root = render(BacklinksPanel, { wsSlug: 'ws', username: 'u', itemSlug: 'target', itemId: 't1' });
		await vi.waitFor(() => expect(root.querySelectorAll('.snippet').length).toBe(2));
		await tick();
		const dots = root.querySelectorAll('[data-testid="stale-body-dot"]');
		expect(dots.length).toBe(1);
		expect(dots[0].closest('li')?.textContent).toContain('Marked');
		expect(dots[0].getAttribute('title')).toBe(STALE_BODY_NOTICE);
		expect(dots[0].getAttribute('aria-label')).toBe(STALE_BODY_NOTICE);
	});
});
