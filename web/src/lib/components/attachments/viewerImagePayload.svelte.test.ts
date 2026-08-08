import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type {
	AttachmentListItem,
	AttachmentListResponse,
	Comment,
	TimelineEntry,
	TimelineResponse,
} from '$lib/types';

/**
 * WHAT THE PRODUCERS PUT ON EACH IMAGE (PLAN-2392 / TASK-2431, T4a).
 *
 * The strip and the timeline no longer mount `Lightbox` directly — they EMIT on
 * the unified attachment surface channel (`notifyAttachmentSurfaceOpen`, T4a) and
 * an `ItemDetail`-owned host does the mounting. So what they hand off is now only
 * observable on that channel: this file spies the emitter and inspects the
 * emitted event instead of standing in for the component (a `vi.mock` is
 * file-scoped, and the behavioural suites for both components drive the REAL bus).
 *
 * The claim under test is unchanged and narrow: every image carries its
 * `mime_type` and the nullable metadata beside it, the `invoker` is the clicked
 * element, the captured `workspaceSlug` is the one the click happened in, and the
 * `index` is right — now asserted about the EMITTED event rather than stubbed
 * props. `mime_type` is what lets a consumer re-state the DR-16 gate over a set it
 * did not build, and `width` / `height` are here for 3b's pixel-based loading
 * policy — no reader today, which is exactly why a test has to hold them. Dropped
 * fields are silent: nothing renders them, nothing type-errors (they are
 * nullable), and the next phase would rediscover the gap by reopening every
 * producer.
 */

// Spy the surface emitter, keeping the rest of the bus real (the producers import
// other exports from this module, and the real registration listeners must stay).
// We do NOT call through to the real `notifyAttachmentSurfaceOpen` — its deep
// snapshot / validation path is exercised by the events unit test, and capturing
// the producer's raw event is exactly the payload this file means to assert.
const surfaceSpy = vi.hoisted(() => vi.fn());
vi.mock('$lib/attachments/events', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/attachments/events')>();
	return {
		...actual,
		notifyAttachmentSurfaceOpen: (e: unknown) => {
			surfaceSpy(e);
		},
	};
});

/** The events emitted on the surface channel so far, most-recent last. */
function surfaceEvents(): any[] {
	return surfaceSpy.mock.calls.map((c) => c[0]);
}

// ─── strip mocks ────────────────────────────────────────────────────────────

const listMock = vi.fn<(ws: string, filters: Record<string, unknown>) => Promise<AttachmentListResponse>>();
const timelineListMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();

class FakeApiError extends Error {
	code: string;
	constructor(code: string) {
		super(code);
		this.code = code;
	}
}

vi.mock('$lib/api/client', () => ({
	PadApiError: FakeApiError,
	api: {
		attachments: {
			list: (ws: string, filters: Record<string, unknown>) => listMock(ws, filters),
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
			delete: vi.fn(),
		},
		timeline: { list: (ws: string, slug: string) => timelineListMock(ws, slug) },
		comments: { create: vi.fn(), update: vi.fn(), delete: vi.fn() },
	},
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: vi.fn() },
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

const PNG = '11111111-1111-4111-8111-111111111111';
const SVG = '22222222-2222-4222-8222-222222222222';
/** Resolves to nothing — the unresolved-MIME case the set must exclude. */
const UNPROBED = '33333333-3333-4333-8333-333333333333';

vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (_ws: string, uuid: string) =>
		Promise.resolve(
			uuid === PNG
				? { status: 'ok' as const, mime: 'image/png', size: 4096 }
				: uuid === SVG
					? { status: 'ok' as const, mime: 'image/svg+xml', size: 512 }
					: { status: 'transient' as const }
		),
	invalidateAttachmentMetadata: vi.fn(),
}));

vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('../timeline/fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemAttachmentStrip } = await import('../items/ItemAttachmentStrip.svelte');
const { default: ItemTimeline } = await import('../timeline/ItemTimeline.svelte');

let target: HTMLElement;
let instance: ReturnType<typeof mount> | undefined;

beforeEach(() => {
	surfaceSpy.mockClear();
	listMock.mockReset();
	timelineListMock.mockReset();
	target = document.body.appendChild(document.createElement('div'));
});

afterEach(() => {
	if (instance) unmount(instance);
	instance = undefined;
	target.remove();
});

async function settle() {
	for (let i = 0; i < 8; i++) {
		await tick();
		flushSync();
	}
}

describe('strip → viewer payload (TASK-2431)', () => {
	function row(over: Partial<AttachmentListItem> & { id: string }): AttachmentListItem {
		return {
			workspace_id: 'ws-1',
			uploaded_by: 'u-1',
			storage_key: `key/${over.id}`,
			content_hash: `hash-${over.id}`,
			mime_type: 'image/png',
			size_bytes: 2048,
			filename: `${over.id}.png`,
			created_at: '2026-08-01T00:00:00Z',
			...over,
		};
	}

	it('threads the full row — mime, size and dimensions — onto every image', async () => {
		listMock.mockResolvedValue({
			attachments: [row({ id: 'img1', size_bytes: 9001, width: 800, height: 600 })],
			total: 1,
			limit: 50,
			offset: 0,
		});
		instance = mount(ItemAttachmentStrip, {
			target,
			props: {
				wsSlug: 'ws',
				username: 'dave',
				itemId: 'item-a',
				canDelete: false,
				hostToken: 'host-1',
			},
		});
		flushSync();
		await settle();

		const tile = target.querySelector<HTMLElement>('.att-tile')!;
		tile.click();
		flushSync();

		const events = surfaceEvents();
		expect(events).toHaveLength(1);
		expect(events[0].images).toEqual([
			{
				id: 'img1',
				alt: 'img1.png',
				filename: 'img1.png',
				mime_type: 'image/png',
				size_bytes: 9001,
				width: 800,
				height: 600,
			},
		]);
		// Captured at emit from the mount's `wsSlug` — never read live from the host.
		expect(events[0].workspaceSlug).toBe('ws');
	});

	it('passes the tile itself as the invoker, so focus returns to it', async () => {
		listMock.mockResolvedValue({
			attachments: [row({ id: 'img1' })],
			total: 1,
			limit: 50,
			offset: 0,
		});
		instance = mount(ItemAttachmentStrip, {
			target,
			props: {
				wsSlug: 'ws',
				username: 'dave',
				itemId: 'item-a',
				canDelete: false,
				hostToken: 'host-1',
			},
		});
		flushSync();
		await settle();

		const tile = target.querySelector<HTMLElement>('.att-tile')!;
		tile.click();
		flushSync();

		// The surface's own fallback ("whatever held focus at open") cannot stand
		// in for this: a click does not focus a button on every engine, and by
		// the time the surface restores, focus may have been anywhere.
		const events = surfaceEvents();
		expect(events[0].invoker).toBe(tile);
		expect(events[0].workspaceSlug).toBe('ws');
	});

	it('emits ONLY allowlisted rows, whatever else the item holds', async () => {
		// The payload assertions above are all safe inputs, so they cannot catch
		// a gate regression on their own — this one feeds the producer an SVG, an
		// undecodable raster and a row with no MIME at all, and pins what comes
		// out the other side (Codex: stub-based payload tests with only safe
		// inputs prove nothing about the gate).
		listMock.mockResolvedValue({
			attachments: [
				row({ id: 'svg', mime_type: 'image/svg+xml', filename: 'logo.svg' }),
				row({ id: 'img1' }),
				row({ id: 'tiff', mime_type: 'image/tiff', filename: 'scan.tiff' }),
				row({ id: 'blank', mime_type: '', filename: 'mystery' }),
				row({ id: 'img2' }),
			],
			total: 5,
			limit: 50,
			offset: 0,
		});
		instance = mount(ItemAttachmentStrip, {
			target,
			props: {
				wsSlug: 'ws',
				username: 'dave',
				itemId: 'item-a',
				canDelete: false,
				hostToken: 'host-1',
			},
		});
		flushSync();
		await settle();

		// Only the two PNGs render as IMAGE tiles (a thumbnail inside the
		// button); the rest take the file branch, which is the same `.att-tile`
		// class with an icon and opens the options panel instead.
		const tiles = Array.from(target.querySelectorAll<HTMLElement>('.att-tile'));
		expect(tiles).toHaveLength(5);
		const imageTiles = tiles.filter((t) => t.querySelector('img') !== null);
		expect(imageTiles).toHaveLength(2);
		imageTiles[0].click();
		flushSync();

		const events = surfaceEvents();
		expect(events).toHaveLength(1);
		expect(events[0].images.map((im: { id: string }) => im.id)).toEqual(['img1', 'img2']);
		expect(events[0].images.every((im: { mime_type: string }) => im.mime_type === 'image/png')).toBe(
			true
		);
		// Opened on the clicked one, at its position in the FILTERED set.
		expect(events[0].index).toBe(0);
	});

	it('leaves dimensions null when the row has none, rather than inventing them', async () => {
		listMock.mockResolvedValue({
			attachments: [row({ id: 'img1', width: null, height: null })],
			total: 1,
			limit: 50,
			offset: 0,
		});
		instance = mount(ItemAttachmentStrip, {
			target,
			props: {
				wsSlug: 'ws',
				username: 'dave',
				itemId: 'item-a',
				canDelete: false,
				hostToken: 'host-1',
			},
		});
		flushSync();
		await settle();

		target.querySelector<HTMLElement>('.att-tile')!.click();
		flushSync();

		const events = surfaceEvents();
		expect(events[0].images[0].width).toBeNull();
		expect(events[0].images[0].height).toBeNull();
	});
});

describe('timeline → viewer payload (TASK-2431)', () => {
	function timelineResponse(): TimelineResponse {
		const comment: Comment = {
			id: 'c1',
			item_id: 'item-a',
			workspace_id: 'ws-1',
			author: 'alice',
			body: `![a diagram](pad-attachment:${PNG})`,
			created_by: 'alice',
			source: 'web',
			created_at: '2026-01-01T00:00:00Z',
			updated_at: '2026-01-01T00:00:00Z',
		};
		const entry: TimelineEntry = {
			id: 'e1',
			kind: 'comment',
			created_at: comment.created_at,
			actor: 'alice',
			source: 'web',
			comment,
		};
		return { entries: [entry], has_more: false };
	}

	it('emits ONLY resolved, allowlisted images — never an SVG or an unprobed id', async () => {
		// Same reason as the strip's: a stub fed only safe inputs cannot fail
		// when the gate does. The body embeds a PNG, an SVG and an id whose probe
		// never resolves; the SVG renders as an <img> (the renderer emits one for
		// any image/*, deliberately) and must still be absent from the set.
		const comment: Comment = {
			id: 'c1',
			item_id: 'item-a',
			workspace_id: 'ws-1',
			author: 'alice',
			body: `![png](pad-attachment:${PNG})\n\n![svg](pad-attachment:${SVG})\n\n![unknown](pad-attachment:${UNPROBED})`,
			created_by: 'alice',
			source: 'web',
			created_at: '2026-01-01T00:00:00Z',
			updated_at: '2026-01-01T00:00:00Z',
		};
		timelineListMock.mockResolvedValue({
			entries: [
				{
					id: 'e1',
					kind: 'comment',
					created_at: comment.created_at,
					actor: 'alice',
					source: 'web',
					comment,
				},
			],
			has_more: false,
		});
		instance = mount(ItemTimeline, {
			target,
			props: {
				wsSlug: 'ws',
				username: 'alice',
				itemSlug: 'TASK-1',
				currentContent: '',
				itemId: 'item-a',
				collectionId: 'coll-1',
			},
		});
		await settle();

		const rendered = Array.from(target.querySelectorAll<HTMLElement>('img[data-attachment-id]'));
		// The SVG IS rendered — if it ever stops being, this test goes vacuous.
		expect(rendered.map((el) => el.getAttribute('data-attachment-id'))).toContain(SVG);

		const png = rendered.find((el) => el.getAttribute('data-attachment-id') === PNG)!;
		png.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
		flushSync();

		const events = surfaceEvents();
		expect(events).toHaveLength(1);
		expect(events[0].images.map((im: { id: string }) => im.id)).toEqual([PNG]);
		expect(events[0].workspaceSlug).toBe('ws');

		// ...and the SVG opens nothing at all.
		surfaceSpy.mockClear();
		const svg = rendered.find((el) => el.getAttribute('data-attachment-id') === SVG)!;
		svg.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
		flushSync();
		expect(surfaceEvents()).toHaveLength(0);
	});

	it('carries the probed mime and size, nulls what HEAD cannot know, and names the invoker', async () => {
		timelineListMock.mockResolvedValue(timelineResponse());
		instance = mount(ItemTimeline, {
			target,
			props: {
				wsSlug: 'ws',
				username: 'alice',
				itemSlug: 'TASK-1',
				currentContent: '',
				itemId: 'item-a',
				collectionId: 'coll-1',
			},
		});
		await settle();

		const thumb = target.querySelector<HTMLElement>('img[data-attachment-id]')!;
		thumb.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
		flushSync();

		const events = surfaceEvents();
		expect(events).toHaveLength(1);
		expect(events[0].images).toEqual([
			{
				id: PNG,
				alt: 'a diagram',
				// The probe deliberately stores no filename (the alt text is the
				// label), and a HEAD response carries no intrinsic dimensions —
				// null is the honest answer, not a placeholder.
				filename: null,
				mime_type: 'image/png',
				size_bytes: 4096,
				width: null,
				height: null,
			},
		]);
		expect(events[0].invoker).toBe(thumb);
		expect(events[0].workspaceSlug).toBe('ws');
	});
});
