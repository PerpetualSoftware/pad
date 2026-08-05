import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Comment, TimelineEntry, TimelineResponse } from '$lib/types';
import { __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';
import { _resetEscapeStackForTests } from '$lib/stores/escapeStack';

/**
 * The timeline's open-the-viewer gate (PLAN-2392 DR-16 / TASK-2431).
 *
 * The hole these cover: the sibling list the viewer's ←/→ page through was
 * built from every `img[data-attachment-id]` in the comment body with NO MIME
 * consulted, while the markdown renderer emits an `<img>` for any `image/*` —
 * so opening a safe PNG and pressing → could land on an `image/svg+xml`.
 * Gating the CLICKED image alone is not a gate, which is why every assertion
 * here is about the whole list and about both activation routes.
 *
 * Deliberately against the REAL `Lightbox` rather than a stub: the claim is
 * "no unsafe image can be REACHED", and only the real component's ←/→ can
 * demonstrate where an arrow key actually lands. The rendered `<img>`'s `src`
 * carries the attachment id, so what the viewer is showing is observable.
 *
 * The markdown pipeline is real too — the `<img>` elements under test are the
 * ones `renderMarkdown` + `sanitizeMarkdownHtml` actually produce.
 */

const PNG_A = '11111111-1111-4111-8111-111111111111';
const SVG = '22222222-2222-4222-8222-222222222222';
const PNG_B = '33333333-3333-4333-8333-333333333333';
const TIFF = '44444444-4444-4444-8444-444444444444';
const UNPROBED = '55555555-5555-4555-8555-555555555555';

/** MIME per attachment id, as the HEAD probe would report it. */
const MIMES: Record<string, string> = {
	[PNG_A]: 'image/png',
	[SVG]: 'image/svg+xml',
	[PNG_B]: 'image/jpeg',
	[TIFF]: 'image/tiff',
};

const timelineListMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: {
			list: (ws: string, slug: string) => timelineListMock(ws, slug),
		},
		comments: {
			create: vi.fn(),
			update: vi.fn(),
			delete: vi.fn(),
			addReaction: vi.fn(),
			removeReaction: vi.fn(),
		},
		attachments: {
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
		},
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'user-1', user: { id: 'user-1', role: 'member' } },
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => false },
}));

// The HEAD probe, answered from MIMES. An id absent from the table resolves as
// `transient` — the "not known" case the gate must fail safe on.
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (_ws: string, uuid: string) =>
		Promise.resolve(
			MIMES[uuid]
				? { status: 'ok' as const, mime: MIMES[uuid], size: 4096 }
				: { status: 'transient' as const }
		),
	invalidateAttachmentMetadata: vi.fn(),
}));

// Tiptap in jsdom is not what these tests are about; the composer is inert.
vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function comment(body: string, id = 'c1'): Comment {
	return {
		id,
		item_id: 'item-a',
		workspace_id: 'ws-1',
		author: 'alice',
		body,
		created_by: 'alice',
		source: 'web',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	};
}

function entry(c: Comment): TimelineEntry {
	return {
		id: `e-${c.id}`,
		kind: 'comment',
		created_at: c.created_at,
		actor: 'alice',
		source: 'web',
		comment: c,
	};
}

/** A body embedding each id as an inline image, in order. */
function bodyWith(ids: string[]): string {
	return ids.map((id, i) => `![image ${i}](pad-attachment:${id})`).join('\n\n');
}

function respond(ids: string[]): TimelineResponse {
	return { entries: [entry(comment(bodyWith(ids)))], has_more: false };
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

// Reactive props object so a test can flip wsSlug / itemSlug the way ItemDetail
// does — the timeline is mounted WITHOUT `{#key}` and is reused across the
// switch, which is the whole point of the lifecycle tests below.
const props = $state<{
	wsSlug: string;
	username: string;
	itemSlug: string;
	currentContent: string;
	itemId: string;
	collectionId: string;
	visibleKinds: Array<'comment' | 'activity' | 'version'> | undefined;
}>({
	wsSlug: 'ws',
	username: 'alice',
	itemSlug: 'TASK-1',
	currentContent: '',
	itemId: 'item-a',
	collectionId: 'coll-1',
	visibleKinds: undefined,
});

function resetProps() {
	props.wsSlug = 'ws';
	props.username = 'alice';
	props.itemSlug = 'TASK-1';
	props.currentContent = '';
	props.itemId = 'item-a';
	props.collectionId = 'coll-1';
	props.visibleKinds = undefined;
}

function render() {
	app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
	return app;
}

/**
 * Let the timeline fetch, probe, render and run its deferred semantics pass.
 * Several microtask hops: list() → entries → probe → attMeta → re-render →
 * `tick()` inside the semantics effect.
 */
async function settle() {
	for (let i = 0; i < 8; i++) {
		await tick();
		flushSync();
	}
}

function thumbs(): HTMLElement[] {
	return Array.from(host.querySelectorAll<HTMLElement>('img[data-attachment-id]'));
}

function thumbFor(id: string): HTMLElement {
	const el = thumbs().find((t) => t.getAttribute('data-attachment-id') === id);
	if (!el) throw new Error(`no thumbnail rendered for ${id}`);
	return el;
}

/** The attachment id the open viewer is currently SHOWING, or null if closed. */
function viewerShowing(): string | null {
	const img = document.querySelector<HTMLImageElement>('.lightbox-backdrop .lightbox-image');
	if (!img) return null;
	const m = /attachments\/([0-9a-f-]+)/.exec(img.getAttribute('src') ?? '');
	return m ? m[1] : null;
}

function viewerOpen(): boolean {
	return document.querySelector('.lightbox-backdrop') !== null;
}

function click(el: HTMLElement) {
	el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
	flushSync();
}

function pressOn(el: HTMLElement, key: string) {
	el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
	flushSync();
}

/** The viewer listens on `window`, so its arrows are driven from there. */
function pressGlobal(key: string) {
	window.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
	flushSync();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	resetProps();
	timelineListMock.mockReset();
	timelineListMock.mockResolvedValue({ entries: [], has_more: false });
});

afterEach(() => {
	// Unmount FIRST: that runs the viewer's own teardown, which is what
	// unregisters its Escape handler and releases its backdrop lease. Ripping
	// the portaled node out beforehand would leave both behind.
	if (app) unmount(app);
	app = null;
	host.remove();
	// The viewer portals to <body>; make sure nothing leaks between tests.
	document.querySelectorAll('.lightbox-backdrop').forEach((n) => n.remove());
	// ...and reset the two module-global registries a viewer touches, so a case
	// that ends with one open cannot colour the next test (the strip suite's
	// afterEach does the same).
	__resetViewerBackdropForTests();
	_resetEscapeStackForTests();
});

describe('ItemTimeline — viewer open gate (TASK-2431)', () => {
	it('renders an <img> for the SVG — the render decision is not the gate', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, SVG]));
		render();
		await settle();

		// If this ever stops holding, the tests below are vacuous: they would be
		// proving that an element nobody renders cannot be clicked.
		expect(thumbs().map((t) => t.getAttribute('data-attachment-id'))).toEqual([PNG_A, SVG]);
	});

	it('refuses a click on a non-allowlisted image', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, SVG]));
		render();
		await settle();

		click(thumbFor(SVG));
		expect(viewerOpen()).toBe(false);
	});

	it('refuses Enter and Space on a non-allowlisted image', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, SVG]));
		render();
		await settle();

		pressOn(thumbFor(SVG), 'Enter');
		expect(viewerOpen()).toBe(false);
		pressOn(thumbFor(SVG), ' ');
		expect(viewerOpen()).toBe(false);
	});

	it('opens on an allowlisted image, by mouse and by keyboard', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, SVG]));
		render();
		await settle();

		click(thumbFor(PNG_A));
		expect(viewerShowing()).toBe(PNG_A);

		// Close and reopen from the keyboard.
		(document.querySelector('.lightbox-close') as HTMLElement).click();
		flushSync();
		expect(viewerOpen()).toBe(false);

		pressOn(thumbFor(PNG_A), 'Enter');
		expect(viewerShowing()).toBe(PNG_A);
	});

	it('never pages onto an unsafe sibling with ←/→', async () => {
		// Mixed: safe, unsafe, safe, undecodable, unprobed.
		timelineListMock.mockResolvedValue(respond([PNG_A, SVG, PNG_B, TIFF, UNPROBED]));
		render();
		await settle();

		click(thumbFor(PNG_A));
		expect(viewerShowing()).toBe(PNG_A);

		// The set is the two allowlisted images ONLY, so → wraps between them
		// and every position is safe. Walk a full cycle in both directions.
		const seen: (string | null)[] = [viewerShowing()];
		for (let i = 0; i < 4; i++) {
			pressGlobal('ArrowRight');
			seen.push(viewerShowing());
		}
		for (let i = 0; i < 4; i++) {
			pressGlobal('ArrowLeft');
			seen.push(viewerShowing());
		}
		expect(seen).toEqual([PNG_A, PNG_B, PNG_A, PNG_B, PNG_A, PNG_B, PNG_A, PNG_B, PNG_A]);
		expect(seen).not.toContain(SVG);
		expect(seen).not.toContain(TIFF);
		expect(seen).not.toContain(UNPROBED);

		// ...and the counter agrees the set has two members, not five.
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
	});

	it('derives the index from the clicked ID, not the DOM position', async () => {
		// PNG_B is DOM index 2 but list index 1 once the SVG is filtered out.
		// A position-derived index would open on the wrong image — and with a
		// leading run of refusals, past the end of the list entirely.
		timelineListMock.mockResolvedValue(respond([SVG, TIFF, PNG_B, PNG_A]));
		render();
		await settle();

		click(thumbFor(PNG_B));
		expect(viewerShowing()).toBe(PNG_B);
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');

		pressGlobal('ArrowRight');
		expect(viewerShowing()).toBe(PNG_A);
	});

	it('fails safe on a thumbnail whose MIME it holds no answer for', async () => {
		// The gate's `!meta` branch, tested where it is REACHABLE rather than
		// through the renderer. Asserting that an unprobed id renders no <img>
		// would prove nothing about the gate — it is a fact about the markdown
		// resolver, and it holds with the gate deleted.
		//
		// The handlers are delegated over the entry list and act on whatever
		// `img[data-attachment-id]` is in the DOM, so an element the component
		// has no metadata for is a real input: a node left over from a body that
		// re-rendered, or one from a future surface. It must be refused, and it
		// must not be a member of a NEIGHBOUR's set either.
		timelineListMock.mockResolvedValue(respond([PNG_A]));
		render();
		await settle();

		const body = host.querySelector('.comment-body, .reply-body')!;
		const stray = document.createElement('img');
		stray.setAttribute('data-attachment-id', UNPROBED);
		stray.setAttribute('alt', 'unknown');
		body.appendChild(stray);

		click(stray);
		expect(viewerOpen()).toBe(false);
		pressOn(stray, 'Enter');
		expect(viewerOpen()).toBe(false);

		// ...and the PNG beside it opens a ONE-image viewer: no counter means no
		// second member, so ← / → cannot page onto the stray.
		click(thumbFor(PNG_A));
		expect(viewerShowing()).toBe(PNG_A);
		expect(document.querySelector('.lightbox-counter')).toBeNull();
	});

	it('admits an image once a later probe resolves its MIME', async () => {
		// The other half of failing safe: refusal is provisional, not a latch.
		MIMES[UNPROBED] = 'image/png';
		try {
			timelineListMock.mockResolvedValue(respond([UNPROBED, PNG_A]));
			render();
			await settle();

			click(thumbFor(UNPROBED));
			expect(viewerShowing()).toBe(UNPROBED);
		} finally {
			delete MIMES[UNPROBED];
		}
	});
});

describe('ItemTimeline — interactive semantics track the gate (TASK-2431)', () => {
	it('marks allowlisted thumbnails as buttons and leaves refused ones inert', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, SVG, TIFF]));
		render();
		await settle();

		const png = thumbFor(PNG_A);
		expect(png.getAttribute('role')).toBe('button');
		expect(png.getAttribute('tabindex')).toBe('0');
		expect(png.getAttribute('aria-label')).toContain('View image');

		for (const refused of [thumbFor(SVG), thumbFor(TIFF)]) {
			// A focus stop announced as a button whose activation does nothing is
			// worse than the hole it replaces.
			expect(refused.getAttribute('role')).toBeNull();
			expect(refused.getAttribute('tabindex')).toBeNull();
			expect(refused.getAttribute('aria-label')).toBeNull();
		}
	});

	it('re-applies them when the kind filter rebuilds the cards', async () => {
		// The pane's Activity / Versions tabs filter the RENDERED set without
		// refetching: `entries` is untouched while every comment card is
		// destroyed and rebuilt. The rebuilt image keeps working by mouse (the
		// listeners are delegated to the container), so a pass that missed this
		// left it openable by mouse and unreachable by keyboard.
		timelineListMock.mockResolvedValue(respond([PNG_A]));
		props.visibleKinds = ['comment'];
		render();
		await settle();
		expect(thumbFor(PNG_A).getAttribute('role')).toBe('button');

		props.visibleKinds = ['activity'];
		flushSync();
		await settle();
		expect(thumbs()).toHaveLength(0);

		props.visibleKinds = ['comment'];
		flushSync();
		await settle();

		const png = thumbFor(PNG_A);
		expect(png.getAttribute('role')).toBe('button');
		expect(png.getAttribute('tabindex')).toBe('0');
		pressOn(png, 'Enter');
		expect(viewerShowing()).toBe(PNG_A);
	});
});

describe('ItemTimeline — A→B viewer lifecycle (TASK-2431)', () => {
	it('closes an open viewer when the workspace switches under the same ref', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, PNG_B]));
		props.wsSlug = 'ws-one';
		render();
		await settle();

		click(thumbFor(PNG_A));
		expect(viewerShowing()).toBe(PNG_A);
		expect(document.querySelector('.lightbox-image')?.getAttribute('src')).toContain('ws-one');

		// Same itemSlug, different workspace — the case the component had no
		// reset for at all. A viewer left up here keeps showing ws-one's ids
		// while the timeline underneath is ws-two's.
		props.wsSlug = 'ws-two';
		flushSync();
		await settle();

		expect(viewerOpen()).toBe(false);
	});

	it('closes an open viewer when the item switches', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, PNG_B]));
		render();
		await settle();

		click(thumbFor(PNG_A));
		expect(viewerOpen()).toBe(true);

		props.itemSlug = 'TASK-2';
		flushSync();
		await settle();

		expect(viewerOpen()).toBe(false);
	});

	it('leaves an open viewer alone when nothing switched', async () => {
		timelineListMock.mockResolvedValue(respond([PNG_A, PNG_B]));
		render();
		await settle();

		click(thumbFor(PNG_A));
		expect(viewerOpen()).toBe(true);

		// A prop that is not part of the view identity must not tear the viewer
		// down — a reset keyed too broadly is its own bug.
		props.username = 'bob';
		flushSync();
		await settle();

		expect(viewerShowing()).toBe(PNG_A);
	});
});
