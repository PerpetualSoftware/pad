import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { AttachmentListItem, AttachmentListResponse } from '$lib/types';
import type { UploadedAttachment } from '$lib/attachments/events';

// TASK-2383. The strip is mounted OUTSIDE ItemDetail's `{#key itemSlug}`
// block, so it PERSISTS across an A→B item switch — the no-{#key} bug class
// from PLAN-2105 / TASK-2112. These tests mount the real component and drive
// its fetch to prove the generation fence holds when A's response resolves
// after B's request went out.

const listMock =
	vi.fn<(ws: string, filters: Record<string, unknown>) => Promise<AttachmentListResponse>>();
const deleteMock = vi.fn<(ws: string, id: string) => Promise<void>>();
const toastMock = vi.fn<(message: string, kind?: string) => void>();

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
			delete: (ws: string, id: string) => deleteMock(ws, id),
		},
	},
}));

const notifyDeletedMock = vi.fn<(uuid: string) => void>();
// A stand-in registry: registerAttachmentDeletionListener is real enough to
// drive the strip's own subscription (broadcastDeletion invokes it), while
// notifyAttachmentDeleted is a pure spy — it records the emit WITHOUT fanning
// out, so a test can assert what the strip announces separately from what it
// receives.
const deletionListeners = new Set<(uuid: string) => void>();
function broadcastDeletion(uuid: string) {
	for (const fn of deletionListeners) fn(uuid);
}
const uploadListeners = new Set<(itemId: string, a: UploadedAttachment) => void>();
function broadcastUpload(itemId: string, a: UploadedAttachment) {
	for (const fn of uploadListeners) fn(itemId, a);
}

// TASK-2424: the strip is now also an EMITTER on the open-panel channel, so
// the mocked module has to carry that export too (a vi.mock factory replaces
// the whole module — a missing export is an import error, not a silent hole).
const panelOpenMock = vi.fn<(event: Record<string, unknown>) => void>();

vi.mock('$lib/attachments/events', () => ({
	announceAttachmentDeleted: (ws: string, uuid: string) => {
		notifyDeletedMock(uuid);
		invalidateMock(ws, uuid);
	},
	notifyAttachmentPanelOpen: (event: Record<string, unknown>) => panelOpenMock(event),
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
	registerAttachmentUploadListener: (fn: (itemId: string, a: UploadedAttachment) => void) => {
		uploadListeners.add(fn);
		return () => uploadListeners.delete(fn);
	},
}));

// announceAttachmentDeleted bundles the notify + cache-invalidate pair; the
// mock above splits them back out so tests can assert each half.
const invalidateMock = vi.fn<(ws: string, uuid: string) => void>();

// The strip also reaches the shared HEAD-metadata cache directly, on Retry
// (PLAN-2392 DR-10) — a separate spy from the events bus's, so the two can be
// asserted independently.
const invalidateMetadataMock = vi.fn<(ws: string, uuid: string) => void>();
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	invalidateAttachmentMetadata: (ws: string, uuid: string) => invalidateMetadataMock(ws, uuid),
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (message: string, kind?: string) => toastMock(message, kind) },
}));

const { default: ItemAttachmentStrip } = await import('./ItemAttachmentStrip.svelte');

function att(overrides: Partial<AttachmentListItem> & { id: string }): AttachmentListItem {
	return {
		workspace_id: 'ws-1',
		uploaded_by: 'u-1',
		storage_key: `key/${overrides.id}`,
		content_hash: `hash-${overrides.id}`,
		mime_type: 'image/png',
		size_bytes: 2048,
		filename: `${overrides.id}.png`,
		created_at: '2026-08-01T00:00:00Z',
		...overrides,
	};
}

function response(attachments: AttachmentListItem[]): AttachmentListResponse {
	// `total` is deliberately inflated in some tests: the +N affordance must
	// derive from the FETCHED ROWS, never from this field (PLAN-2382 DR-9).
	return { attachments, total: attachments.length, limit: 50, offset: 0 };
}

/** A promise plus its resolver, so a test can control when a fetch lands. */
function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((r) => (resolve = r));
	return { promise, resolve };
}

// Reactive props object so a test can flip itemId the way ItemDetail's
// persistent (un-{#key}'d) mount point does. Declared once at the top level
// because `$state(...)` may only initialize a declaration.
const props = $state<{
	wsSlug: string;
	username: string;
	itemId: string | null;
	canDelete: boolean;
	itemContent: string | null;
	liveContent: (() => string | null) | null;
	hostToken: string;
}>({
	wsSlug: 'ws',
	username: 'dave',
	itemId: null,
	canDelete: false,
	itemContent: null,
	liveContent: null,
	hostToken: 'host-1',
});

describe('ItemAttachmentStrip', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;

	beforeEach(() => {
		listMock.mockReset();
		deleteMock.mockReset();
		deleteMock.mockResolvedValue(undefined);
		toastMock.mockReset();
		notifyDeletedMock.mockReset();
		invalidateMock.mockReset();
		invalidateMetadataMock.mockReset();
		panelOpenMock.mockReset();
		props.hostToken = 'host-1';
		props.wsSlug = 'ws';
		props.username = 'dave';
		props.itemId = null;
		props.canDelete = false;
		props.itemContent = null;
		props.liveContent = null;
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		if (instance) unmount(instance);
		instance = undefined;
		target.remove();
	});

	function mountStrip(itemId: string | null) {
		props.itemId = itemId;
		instance = mount(ItemAttachmentStrip, { target, props });
		flushSync();
	}

	function tiles(): HTMLElement[] {
		return Array.from(target.querySelectorAll<HTMLElement>('.att-tile'));
	}

	async function settle() {
		await Promise.resolve();
		await Promise.resolve();
		flushSync();
	}

	it('renders nothing at all when the item has no attachments', async () => {
		listMock.mockResolvedValue(response([]));
		mountStrip('item-a');
		await settle();

		expect(target.querySelector('.attachment-strip')).toBeNull();
		// No element at all — an empty wrapper would still take the parent
		// flex column's `gap` and leave a hole above the editor.
		expect(target.children).toHaveLength(0);
		expect(target.textContent?.trim()).toBe('');
	});

	it('renders nothing while the item id is still unknown, and does not fetch', async () => {
		mountStrip(null);
		await settle();

		expect(listMock).not.toHaveBeenCalled();
		expect(target.querySelector('.attachment-strip')).toBeNull();
	});

	it('fetches by item_id with the 50-row bound and renders a tile per row', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();

		expect(listMock).toHaveBeenCalledWith('ws', { item_id: 'item-a', limit: 50 });
		expect(tiles()).toHaveLength(2);
		expect(target.querySelector('.fields-header')?.textContent).toBe('Attachments · 2');
	});

	// TASK-2424 (PLAN-2392 DR-1 / DR-12) deliberately falsifies the previous
	// version of this test, which pinned the non-image tile as an
	// `<a href download>`: a tap used to put the file straight in Downloads.
	// The tile is now a real button that opens the options panel, and Download
	// is a deliberate choice inside it.
	it('renders non-image tiles as panel-trigger buttons, not download links', async () => {
		listMock.mockResolvedValue(
			response([
				att({ id: 'doc', mime_type: 'application/pdf', filename: 'spec.pdf', size_bytes: 1536 }),
			])
		);
		mountStrip('item-a');
		await settle();

		const tile = tiles()[0];
		expect(tile.tagName).toBe('BUTTON');
		expect(tile.getAttribute('type')).toBe('button');
		// Nothing downloads on tap any more.
		expect(tile.getAttribute('href')).toBeNull();
		expect(tile.getAttribute('download')).toBeNull();
		// The accessible name carries filename, TYPE and the ACTION (DR-12) —
		// the only signpost for the changed behaviour, since DR-1 adds no `⋯`.
		expect(tile.getAttribute('aria-label')).toBe('Options for spec.pdf, PDF, 1.5 KB');
		expect(tile.getAttribute('title')).toBe('spec.pdf, PDF, 1.5 KB');
	});

	it('emits the open-panel event with the anchor and all three metadata fields', async () => {
		listMock.mockResolvedValue(
			response([
				att({ id: 'doc', mime_type: 'application/pdf', filename: 'spec.pdf', size_bytes: 1536 }),
			])
		);
		mountStrip('item-a');
		await settle();

		const tile = tiles()[0];
		tile.click();
		flushSync();

		expect(panelOpenMock).toHaveBeenCalledTimes(1);
		expect(panelOpenMock).toHaveBeenCalledWith({
			attachmentId: 'doc',
			// Routing: which ItemDetail mount shows the panel (DR-8).
			itemId: 'item-a',
			hostToken: 'host-1',
			anchor: tile,
			// The strip always has all three from its list row, unlike a chip.
			filename: 'spec.pdf',
			mime_type: 'application/pdf',
			size_bytes: 1536,
		});
	});

	it('activates exactly once per key press — no keydown handler racing the click', async () => {
		listMock.mockResolvedValue(
			response([att({ id: 'doc', mime_type: 'application/pdf', filename: 'spec.pdf' })])
		);
		mountStrip('item-a');
		await settle();

		const tile = tiles()[0] as HTMLButtonElement;
		// DR-12 wants Enter AND Space to activate, exactly once each. A native
		// <button> is how that is guaranteed: the UA converts both keys into a
		// single `click` and already suppresses Space's page scroll. The thing
		// a test can actually falsify is the failure mode DR-12 names — a
		// hand-rolled keydown handler firing ALONGSIDE the UA's click, opening
		// the panel twice. jsdom does not synthesise the activation click, so
		// the key press alone must produce nothing...
		for (const key of ['Enter', ' ']) {
			const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
			tile.dispatchEvent(event);
			flushSync();
			expect(panelOpenMock).not.toHaveBeenCalled();
			// ...and Space must not be swallowed as a scroll suppressor either:
			// the UA's own default handling is what we are relying on.
			expect(event.defaultPrevented).toBe(false);
		}
		// The UA's click is then the one and only activation path.
		tile.click();
		flushSync();
		expect(panelOpenMock).toHaveBeenCalledTimes(1);
	});

	it('renders an SVG as a FILE tile and keeps it out of the viewer (DR-16)', async () => {
		// `isImage` would say yes to this — the viewer takes an exact raster
		// allowlist instead, because SVG can carry active content.
		listMock.mockResolvedValue(
			response([
				att({ id: 'svg', mime_type: 'image/svg+xml', filename: 'logo.svg' }),
				att({ id: 'png', mime_type: 'image/png', filename: 'shot.png' }),
			])
		);
		mountStrip('item-a');
		await settle();

		const [svgTile, pngTile] = tiles();
		// The SVG got the file path: an icon + name, no thumbnail request.
		expect(svgTile.querySelector('img')).toBeNull();
		expect(svgTile.getAttribute('aria-label')).toContain('Options for logo.svg');

		svgTile.click();
		flushSync();
		expect(document.querySelector('.lightbox-backdrop')).toBeNull();
		expect(panelOpenMock).toHaveBeenCalledTimes(1);

		// ...and it isn't a member of the lightbox's set either, so the PNG's
		// viewer holds ONE image rather than paging into the SVG (the counter
		// only renders for a multi-image set, so its absence IS the assertion).
		panelOpenMock.mockClear();
		pngTile.click();
		flushSync();
		expect(panelOpenMock).not.toHaveBeenCalled();
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();
		expect(document.querySelector('.lightbox-counter')).toBeNull();
	});

	it('renders images as thumb-sm buttons that open the lightbox', async () => {
		listMock.mockResolvedValue(response([att({ id: 'img1' }), att({ id: 'img2' })]));
		mountStrip('item-a');
		await settle();

		const tile = tiles()[1];
		expect(tile.tagName).toBe('BUTTON');
		expect(tile.querySelector('img')?.getAttribute('src')).toBe(
			'/api/v1/workspaces/ws/attachments/img2?variant=thumb-sm'
		);

		tile.click();
		flushSync();
		// The lightbox opens on the clicked image, with both images available
		// so ←/→ page through the item's attachments.
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('2 / 2');
	});

	it('opens the lightbox at the IMAGE index, not the attachment index', async () => {
		// Interleaved non-images: a naive `attachments.indexOf(att)` would open
		// the wrong image, since the lightbox only ever receives image rows.
		listMock.mockResolvedValue(
			response([
				att({ id: 'img1' }),
				att({ id: 'pdf', mime_type: 'application/pdf', filename: 'a.pdf' }),
				att({ id: 'zip', mime_type: 'application/zip', filename: 'a.zip' }),
				att({ id: 'img2' }),
			])
		);
		mountStrip('item-a');
		await settle();

		// Fourth tile overall, but the SECOND image.
		tiles()[3].click();
		flushSync();
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('2 / 2');
		expect(document.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt')).toBe(
			'img2.png'
		);

		// ← wraps to the first image (the non-images are absent from the set).
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft' }));
		flushSync();
		expect(document.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt')).toBe(
			'img1.png'
		);

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		flushSync();
		expect(document.querySelector('.lightbox-backdrop')).toBeNull();
	});

	it('collapses past 8 tiles behind a +N chip derived from fetched rows', async () => {
		const rows = Array.from({ length: 12 }, (_, i) => att({ id: `a${i}` }));
		// `total` claims far more than was fetched — the chip must ignore it.
		listMock.mockResolvedValue({ attachments: rows, total: 999, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();

		expect(tiles()).toHaveLength(8);
		const more = target.querySelector<HTMLElement>('.att-more-expand');
		expect(more?.textContent?.trim()).toBe('+4');

		more?.click();
		flushSync();
		expect(tiles()).toHaveLength(12);
		expect(target.querySelector('.att-more-expand')).toBeNull();
	});

	it('offers an item-scoped "View all" continuation at the 50-row bound', async () => {
		// PLAN-2392 DR-18: the continuation is offered WITHOUT expanding first,
		// carries the item's real total, and lands on this item's attachments
		// rather than the workspace-wide Storage list.
		const rows = Array.from({ length: 50 }, (_, i) => att({ id: `a${i}` }));
		listMock.mockResolvedValue({ attachments: rows, total: 120, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();

		// The header can't claim 120 — expanding reaches only the 50 fetched —
		// so it says "50+" and the exact figure lives on the link.
		expect(target.querySelector('.fields-header')?.textContent?.trim()).toBe(
			'Attachments · 50+'
		);
		expect(target.querySelector<HTMLElement>('.att-more-expand')?.textContent?.trim()).toBe(
			'+42'
		);

		const link = target.querySelector<HTMLAnchorElement>('a.att-more-link');
		expect(link?.textContent?.trim()).toBe('View all (120)');
		expect(link?.getAttribute('href')).toBe('/dave/ws/settings?attachment_item=item-a#storage');
	});

	it('never grows the in-memory list past the 50-row bound (DR-11)', async () => {
		// The documented cap used to bound only the fetch `limit`; the upload
		// path prepended unconditionally, so a long paste session grew the list
		// (and the lightbox set) without limit.
		const rows = Array.from({ length: 50 }, (_, i) => att({ id: `a${i}` }));
		listMock.mockResolvedValue({ attachments: rows, total: 50, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();

		for (let i = 0; i < 20; i++) broadcastUpload('item-a', uploaded(`paste-${i}`));
		flushSync();

		const more = target.querySelector<HTMLElement>('.att-more-expand');
		more?.click();
		flushSync();
		expect(tiles()).toHaveLength(50);
		// Newest-first, so the cap sheds the oldest rows, not the fresh ones.
		expect(tiles()[0].getAttribute('aria-label')).toContain('paste-19.png');
	});

	it('bounds the pending-upload buffer too, not just the merged result', async () => {
		// Uploads announced while the list request is in flight go into
		// `pendingUploads`, which was itself unbounded — so bounding only the
		// merge still let the buffer grow across a long paste session
		// (DR-11, Codex rounds 4 and 6). Proven through the merge: 60 pending
		// uploads must yield 50 rows, not 60 (and not 110 once rows land).
		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValue(pending.promise);
		mountStrip('item-a');
		flushSync();

		for (let i = 0; i < 60; i++) broadcastUpload('item-a', uploaded(`p${i}`));
		flushSync();

		// The GET was issued before the uploads existed, so both its page and
		// its `total` predate them.
		pending.resolve({
			attachments: [att({ id: 'server1' }), att({ id: 'server2' })],
			total: 2,
			limit: 50,
			offset: 0,
		});
		await settle();

		target.querySelector<HTMLElement>('.att-more-expand')?.click();
		flushSync();
		expect(tiles()).toHaveLength(50);
		// The rows the merge cap shed still exist, so the continuation counts
		// them rather than pretending the strip holds everything. The 10 the
		// PENDING buffer shed are the documented approximation — counting them
		// would double-count the ordinary case where the response's `total`
		// already includes the uploads; the next load corrects it.
		expect(
			target.querySelector<HTMLAnchorElement>('a.att-more-link')?.textContent?.trim()
		).toBe('View all (52)');
	});

	it('still offers the continuation when every held row is deleted', async () => {
		// 50 held + 70 past the bound. Deleting the held ones must not hide the
		// section outright — there are still 70 attachments on this item, and
		// the link is the only way to reach them (Codex round 5).
		const rows = Array.from({ length: 50 }, (_, i) => att({ id: `a${i}` }));
		listMock.mockResolvedValue({ attachments: rows, total: 120, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();

		for (const r of rows) broadcastDeletion(r.id);
		flushSync();

		expect(tiles()).toHaveLength(0);
		expect(target.querySelector('.attachment-strip')).not.toBeNull();
		expect(target.querySelector('.fields-header')?.textContent?.trim()).toBe('Attachments · 70');
		expect(
			target.querySelector<HTMLAnchorElement>('a.att-more-link')?.textContent?.trim()
		).toBe('View all (70)');
	});

	it('does not double-count uploads the response already reported', async () => {
		// The GET went out after the uploads, so its `total` already includes
		// them. Adding a separate "pending overflow" on top produced
		// "View all (70)" for 60 real attachments (Codex round 3).
		const rows = Array.from({ length: 50 }, (_, i) => att({ id: `u${i}` }));
		listMock.mockResolvedValue({ attachments: rows, total: 60, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();

		for (let i = 0; i < 10; i++) broadcastUpload('item-a', uploaded(`u${i}`));
		flushSync();

		expect(
			target.querySelector<HTMLAnchorElement>('a.att-more-link')?.textContent?.trim()
		).toBe('View all (60)');
	});

	it('does not count a row deleted while the request was in flight', async () => {
		// `total` predates the deletion, so trusting it verbatim advertised
		// "View all (2)" for a single remaining attachment (Codex round 3).
		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValue(pending.promise);
		mountStrip('item-a');
		flushSync();

		broadcastDeletion('gone');
		flushSync();

		pending.resolve({
			attachments: [att({ id: 'gone' }), att({ id: 'stays' })],
			total: 2,
			limit: 50,
			offset: 0,
		});
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(target.querySelector('a.att-more-link')).toBeNull();
		expect(target.querySelector('.fields-header')?.textContent?.trim()).toBe('Attachments · 1');
	});

	it('never paints item A attachments under item B (generation fence)', async () => {
		const a = deferred<AttachmentListResponse>();
		const b = deferred<AttachmentListResponse>();
		listMock.mockImplementationOnce(() => a.promise).mockImplementationOnce(() => b.promise);

		mountStrip('item-a');
		await settle();
		expect(listMock).toHaveBeenCalledTimes(1);

		// Switch to B while A is still in flight.
		props.itemId = 'item-b';
		flushSync();
		expect(listMock).toHaveBeenCalledTimes(2);

		// A's response lands LATE, after the switch. It must be discarded.
		a.resolve(response([att({ id: 'from-a' })]));
		await settle();
		expect(target.querySelector('.attachment-strip')).toBeNull();

		// B's response paints normally.
		b.resolve(response([att({ id: 'from-b' })]));
		await settle();
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('from-b.png');
	});

	it('clears the previous item tiles immediately on switch', async () => {
		listMock.mockResolvedValueOnce(response([att({ id: 'from-a' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(1);

		// B's fetch never resolves; the strip must still not show A's tiles.
		listMock.mockImplementationOnce(() => deferred<AttachmentListResponse>().promise);
		props.itemId = 'item-b';
		flushSync();
		expect(target.querySelector('.attachment-strip')).toBeNull();
	});

	it('clears when the parent nulls the id mid-switch, then paints B', async () => {
		// The real parent lifecycle (Codex round 1): ItemDetail RETAINS the
		// previous item while B's request is in flight, so it gates the prop on
		// `itemMatchesRef` — the strip sees A → null → B, not A → B.
		listMock.mockResolvedValueOnce(response([att({ id: 'from-a' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(1);

		props.itemId = null;
		flushSync();
		expect(target.querySelector('.attachment-strip')).toBeNull();

		listMock.mockResolvedValueOnce(response([att({ id: 'from-b' })]));
		props.itemId = 'item-b';
		await settle();
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('from-b.png');
		// Only A's and B's fetches — the null pass must not hit the API.
		expect(listMock).toHaveBeenCalledTimes(2);
	});

	it('discards a response that lands after unmount', async () => {
		const pending = deferred<AttachmentListResponse>();
		listMock.mockImplementationOnce(() => pending.promise);
		mountStrip('item-a');
		await settle();

		unmount(instance!);
		instance = undefined;

		// Resolving into a destroyed instance must be a no-op, not a throw.
		pending.resolve(response([att({ id: 'late' })]));
		await settle();
		expect(target.querySelector('.attachment-strip')).toBeNull();
	});

	// ── Load failure (PLAN-2392 DR-10) ────────────────────────────────────
	//
	// This deliberately REPLACES the old "renders nothing when the fetch
	// fails" expectation. Swallowing the error made a broken strip and an
	// empty one indistinguishable, which is exactly what DR-10 forbids.

	function errorRow(): HTMLElement | null {
		return target.querySelector<HTMLElement>('.att-error');
	}

	it('shows a loading row for a slow fetch — loading, empty and failed differ', async () => {
		vi.useFakeTimers();
		try {
			const pending = deferred<AttachmentListResponse>();
			listMock.mockReturnValue(pending.promise);
			mountStrip('item-a');
			flushSync();

			// Inside the grace period the strip is still invisible, so the
			// common no-attachments item never flashes a block above the editor.
			expect(target.querySelector('.attachment-strip')).toBeNull();

			vi.advanceTimersByTime(250);
			flushSync();
			expect(target.querySelector('.att-status')?.textContent).toContain('Loading attachments');
			expect(errorRow()).toBeNull();

			pending.resolve(response([]));
			await Promise.resolve();
			await Promise.resolve();
			flushSync();
			// Empty: back to nothing at all (DR-18).
			expect(target.querySelector('.attachment-strip')).toBeNull();
		} finally {
			vi.useRealTimers();
		}
	});

	it('drops the loading row when the item switches away mid-load', async () => {
		vi.useFakeTimers();
		try {
			listMock.mockReturnValue(deferred<AttachmentListResponse>().promise);
			mountStrip('item-a');
			flushSync();

			props.itemId = null;
			flushSync();
			// A's timer must not fire into B's (here: no) item.
			vi.advanceTimersByTime(250);
			flushSync();
			expect(target.querySelector('.attachment-strip')).toBeNull();
		} finally {
			vi.useRealTimers();
		}
	});

	it('renders a distinguishable, retryable error when the fetch fails', async () => {
		listMock.mockRejectedValue(new Error('boom'));
		mountStrip('item-a');
		await settle();

		expect(target.querySelector('.attachment-strip')).not.toBeNull();
		expect(errorRow()?.textContent).toContain("Couldn't load attachments");
		expect(errorRow()?.querySelector('.att-retry')).not.toBeNull();
		// Failed is not empty: no tiles, but the section is present and says so.
		expect(tiles()).toHaveLength(0);
	});

	it('refetches on Retry and clears the error on success', async () => {
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();
		expect(errorRow()).not.toBeNull();

		listMock.mockResolvedValueOnce(response([att({ id: 'back' })]));
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		expect(listMock).toHaveBeenCalledTimes(2);
		expect(errorRow()).toBeNull();
		expect(tiles()).toHaveLength(1);
		// The per-attachment HEAD cache latches `null` on failure for the page
		// lifetime, so a Retry that doesn't clear it replays the cached failure
		// on every other surface that probed during the same outage (DR-10).
		expect(invalidateMetadataMock).toHaveBeenCalledWith('ws', 'back');
	});

	it('shows the error again when Retry fails too', async () => {
		listMock.mockRejectedValue(new Error('still offline'));
		mountStrip('item-a');
		await settle();

		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		expect(listMock).toHaveBeenCalledTimes(2);
		expect(errorRow()).not.toBeNull();
	});

	it('keeps optimistic uploads across a Retry that fails again', async () => {
		// A Retry is the same item reloading, not a switch — so it must not
		// wipe the rows the first failure deliberately preserved. The upload
		// succeeded; only the listing didn't.
		listMock.mockRejectedValue(new Error('offline'));
		mountStrip('item-a');
		await settle();
		broadcastUpload('item-a', uploaded('survivor'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		expect(errorRow()).not.toBeNull();
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('survivor.png');
	});

	it('ignores a Retry click that lands after the item already switched', async () => {
		// Props update synchronously, effects flush later — so a click can land
		// on item A's still-painted Retry when `itemId` already reads B. Taking
		// it at face value would record a retry for B and preserve A's rows
		// across the switch (Codex round 11).
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();
		broadcastUpload('item-a', uploaded('a-only'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		// No flushSync between the switch and the click: that IS the window.
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a-only'))).toBe(false);
		expect(names.some((n) => n.includes('b1.png'))).toBe(true);
		expect(errorRow()).toBeNull();
	});

	it('ignores a Retry click that lands after the WORKSPACE already switched', async () => {
		// Same stale-click window as the item-switch case, one prop over: the
		// painted error belongs to the old workspace, so honouring the click
		// would carry its rows (and its tombstones) into the new one.
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();
		broadcastUpload('item-a', uploaded('ws1-only'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		listMock.mockResolvedValueOnce(response([att({ id: 'ws2-row' })]));
		props.wsSlug = 'ws2';
		// No flushSync between the switch and the click: that IS the window.
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('ws1-only'))).toBe(false);
		expect(names.some((n) => n.includes('ws2-row.png'))).toBe(true);
		expect(errorRow()).toBeNull();
	});

	it('does not carry a failure across an item switch', async () => {
		listMock.mockRejectedValueOnce(new Error('boom'));
		mountStrip('item-a');
		await settle();
		expect(errorRow()).not.toBeNull();

		listMock.mockResolvedValueOnce(response([]));
		props.itemId = 'item-b';
		flushSync();
		await settle();

		// B has no attachments: no error, and no section at all (DR-18).
		expect(target.querySelector('.attachment-strip')).toBeNull();
	});

	it('does not paint item A failure under item B (generation fence)', async () => {
		let failA!: (err: Error) => void;
		listMock.mockReturnValueOnce(
			new Promise<AttachmentListResponse>((_, reject) => {
				failA = reject;
			})
		);
		mountStrip('item-a');
		await settle();

		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		flushSync();
		await settle();

		failA(new Error('late failure'));
		await settle();

		expect(errorRow()).toBeNull();
		expect(tiles()).toHaveLength(1);
	});


	// ── Delete (TASK-2384) ────────────────────────────────────────────────
	//
	// The affordance is gated on ItemDetail's `mutationsEnabled`
	// (canEdit && !peeking) per PLAN-2382 DR-6, and the confirm text has to
	// stay honest about what was actually checked (DR-5): "referenced in this
	// item's content" is knowable client-side; "unused anywhere" is not.

	function deleteButtons(): HTMLButtonElement[] {
		return Array.from(target.querySelectorAll<HTMLButtonElement>('.att-delete'));
	}

	it('offers no delete control when canDelete is false', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = false;
		mountStrip('item-a');
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(deleteButtons()).toHaveLength(0);
	});

	it('renders a keyboard-reachable delete control per tile when canDelete', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const buttons = deleteButtons();
		expect(buttons).toHaveLength(2);
		expect(buttons[0].getAttribute('aria-label')).toBe('Delete a1.png');
		// Actually focusable — not merely present. The earlier assertion checked
		// the `hidden` PROPERTY, which a `visibility: hidden` rule never sets,
		// so it passed while the control was in fact unreachable by keyboard
		// (Codex round 4). jsdom doesn't apply the component's scoped CSS, so
		// this can't catch a future regression to visibility/display on its own
		// — the browser-level guarantee is the TASK-2385 e2e.
		buttons[0].focus();
		expect(document.activeElement).toBe(buttons[0]);
		expect(buttons[0].disabled).toBe(false);
	});

	it('warns that the attachment is still used in this item content', async () => {
		// A canonical UUID: attachmentRefsIn() is anchored to that shape (the
		// ids the upload endpoint returns), so the reference scan only matches
		// real ids — a short fixture id would silently miss.
		const uuid = '0f9c2f7a-1b2c-4d5e-8f90-1a2b3c4d5e6f';
		listMock.mockResolvedValue(response([att({ id: uuid })]));
		props.canDelete = true;
		props.itemContent = `text ![shot](pad-attachment:${uuid}) more`;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false);
		deleteButtons()[0].click();
		await settle();

		expect(confirmSpy).toHaveBeenCalledOnce();
		expect(confirmSpy.mock.calls[0][0]).toContain("still used in this item's content");
		// Declined → nothing deleted, tile stays.
		expect(deleteMock).not.toHaveBeenCalled();
		expect(tiles()).toHaveLength(1);
		confirmSpy.mockRestore();
	});

	it('never claims an unreferenced attachment is unused', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		props.itemContent = 'no references here';
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false);
		deleteButtons()[0].click();
		await settle();

		const message = String(confirmSpy.mock.calls[0][0]);
		// Comment bodies and other items are NOT scanned client-side (DR-5),
		// so the copy must hedge rather than assert non-use.
		expect(message).toContain('may still be referenced');
		expect(message).not.toContain('not used');
		confirmSpy.mockRestore();
	});

	it('removes the tile optimistically and calls the API on confirm', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		await settle();

		expect(deleteMock).toHaveBeenCalledWith('ws', 'a1');
		expect(tiles()).toHaveLength(1);
		expect(toastMock).not.toHaveBeenCalled();
		// An <img> already painted in the editor never re-requests, so the
		// NodeView has to be told or the body keeps showing a deleted image
		// until reload (Codex round 12).
		expect(notifyDeletedMock).toHaveBeenCalledWith('a1');
		expect(invalidateMock).toHaveBeenCalledWith('ws', 'a1');
		confirmSpy.mockRestore();
	});

	it('refuses a delete click that lands after the ITEM already switched', async () => {
		// The most serious hole the final review found. Props update
		// synchronously and the load effect repaints later, so a click can land
		// on item A's still-mounted tile when `itemId` already reads B — and
		// unlike the rollback, a DELETE cannot be taken back once sent. The
		// fence therefore has to be at ENTRY, against the identity the tile was
		// PAINTED under, not against the live props (which already say B).
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();
		expect(deleteButtons()).toHaveLength(1);

		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		props.itemId = 'item-b';
		// No flushSync between the switch and the click: that IS the window.
		deleteButtons()[0].click();
		await settle();

		// Not even prompted — the tile the user aimed at no longer exists.
		expect(confirmSpy).not.toHaveBeenCalled();
		expect(deleteMock).not.toHaveBeenCalled();
		expect(notifyDeletedMock).not.toHaveBeenCalled();
		expect(tiles()[0].getAttribute('aria-label')).toContain('b1.png');
		confirmSpy.mockRestore();
	});

	it('refuses a delete click that lands after the WORKSPACE already switched', async () => {
		// Same window, one prop over. `wsSlug` is reactive and the strip
		// survives a workspace change, so an item-only entry fence would send
		// the DELETE to the NEW workspace for the OLD workspace's row.
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		listMock.mockResolvedValueOnce(response([att({ id: 'ws2-row' })]));
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		props.wsSlug = 'ws2';
		deleteButtons()[0].click();
		await settle();

		expect(confirmSpy).not.toHaveBeenCalled();
		expect(deleteMock).not.toHaveBeenCalled();
		expect(tiles()[0].getAttribute('aria-label')).toContain('ws2-row.png');
		confirmSpy.mockRestore();
	});

	it('rolls the tile back and toasts when the delete fails', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		deleteMock.mockRejectedValue(new Error('403'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		await settle();

		expect(tiles()).toHaveLength(2);
		// Nothing was deleted server-side, so the editor must NOT be told.
		expect(notifyDeletedMock).not.toHaveBeenCalled();
		expect(toastMock).toHaveBeenCalledOnce();
		expect(String(toastMock.mock.calls[0][0])).toContain('a1.png');
		confirmSpy.mockRestore();
	});

	it('does not roll a failed delete back into a DIFFERENT item strip', async () => {
		// A→B switch while A's delete is in flight. The rollback must not
		// resurrect A's tile under B, and B must not get A's error toast.
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		// A REJECTING deferred: resolving it would skip the catch entirely and
		// the test would pass against a broken rollback (Codex round 2 P3).
		let failDelete!: (err: Error) => void;
		deleteMock.mockReturnValue(
			new Promise<void>((_, reject) => {
				failDelete = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		flushSync();
		expect(tiles()).toHaveLength(0); // optimistic removal happened

		// Switch to B before the delete settles.
		listMock.mockResolvedValue(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		flushSync();
		await settle();

		failDelete(new Error('403'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label'));
		expect(names.some((n) => n?.includes('a1.png'))).toBe(false);
		expect(names.some((n) => n?.includes('b1.png'))).toBe(true);
		expect(toastMock).not.toHaveBeenCalled();
		confirmSpy.mockRestore();
	});

	it('rolls back only the failed row, never resurrecting a concurrent success', async () => {
		// Delete A (will fail) then B (succeeds). A snapshot-based rollback
		// would restore the whole pre-delete array and bring B back from the
		// dead (Codex round 2 P2).
		listMock.mockResolvedValue(
			response([att({ id: 'a1' }), att({ id: 'b1' }), att({ id: 'c1' })])
		);
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		let failFirst!: (err: Error) => void;
		deleteMock.mockReturnValueOnce(
			new Promise<void>((_, reject) => {
				failFirst = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);

		deleteButtons()[0].click(); // a1 — in flight, will fail
		flushSync();
		deleteButtons()[0].click(); // now b1 — resolves immediately
		await settle();

		failFirst(new Error('boom'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(true); // restored
		expect(names.some((n) => n.includes('b1.png'))).toBe(false); // stays deleted
		// ...and restored at its original position, not appended.
		expect(names[0]).toContain('a1.png');
		confirmSpy.mockRestore();
	});

	it('still announces the deletion when the delete 404s', async () => {
		// A 404 proves the row is gone just as well as a 204 does, so the other
		// local surfaces need telling either way (Codex round 19).
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		deleteMock.mockRejectedValue(new FakeApiError('not_found'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		await settle();

		expect(notifyDeletedMock).toHaveBeenCalledWith('a1');
		expect(invalidateMock).toHaveBeenCalledWith('ws', 'a1');
		confirmSpy.mockRestore();
	});

	it('still announces a 404 delete when the view switched under it', async () => {
		// A 404 is proof the row is gone — a fact about the WORKSPACE, not about
		// this view. Fencing the whole catch block on `viewChanged` meant a
		// switch mid-delete swallowed that proof, leaving every other mounted
		// surface (editor NodeViews, the other pane's strip, the HEAD cache)
		// stale with nothing left to correct them (final review round 2). Only
		// the LOCAL reconciliation — rollback, toast — is view-scoped.
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		let failDelete!: (err: Error) => void;
		deleteMock.mockReturnValue(
			new Promise<void>((_, reject) => {
				failDelete = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		flushSync();

		listMock.mockResolvedValue(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		flushSync();
		await settle();

		failDelete(new FakeApiError('not_found'));
		await settle();

		expect(notifyDeletedMock).toHaveBeenCalledWith('a1');
		expect(invalidateMock).toHaveBeenCalledWith('ws', 'a1');
		// ...and still nothing local: no rollback into B's strip, no toast.
		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(false);
		expect(names.some((n) => n.includes('b1.png'))).toBe(true);
		expect(toastMock).not.toHaveBeenCalled();
		confirmSpy.mockRestore();
	});

	it('does not roll back a failed delete that another surface already announced', async () => {
		// Our DELETE fails, but someone else confirmed the same uuid is gone
		// while it was in flight. The tombstone wins — restoring the tile would
		// contradict a deletion we know landed (Codex round 18/19).
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		let failDelete!: (err: Error) => void;
		deleteMock.mockReturnValue(
			new Promise<void>((_, reject) => {
				failDelete = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		flushSync();

		broadcastDeletion('a1');
		flushSync();

		failDelete(new Error('500'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(false);
		confirmSpy.mockRestore();
	});

	it('keeps the tile removed when the delete 404s (already gone)', async () => {
		// Someone else deleted it and this strip has no live subscription, so
		// the tile was stale before the click. Restoring it would leave a dead
		// tile whose download and delete both fail (Codex round 6).
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		deleteMock.mockRejectedValue(new FakeApiError('not_found'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(toastMock).not.toHaveBeenCalled();
		confirmSpy.mockRestore();
	});

	it('names permission as the reason on a 403, and restores the tile', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		deleteMock.mockRejectedValue(new FakeApiError('forbidden'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(String(toastMock.mock.calls[0][0])).toContain("don't have permission");
		confirmSpy.mockRestore();
	});

	it('still rolls back and toasts when a Retry re-ran the load mid-delete', async () => {
		// A Retry is the SAME item reloading, not a switch. Fencing mutations on
		// the load generation made a Retry look like an A→B switch, so a delete
		// failure landing during one was swallowed — no rollback, no toast, no
		// broadcast (final-review P2). Reachable because a failed load still
		// paints rows uploaded while it was in flight, delete control and all.
		listMock.mockRejectedValue(new Error('offline'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();
		broadcastUpload('item-a', uploaded('survivor'));
		flushSync();
		expect(tiles()).toHaveLength(1);
		expect(errorRow()).not.toBeNull();

		let failDelete!: (err: Error) => void;
		deleteMock.mockReturnValue(
			new Promise<void>((_, reject) => {
				failDelete = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		flushSync();
		expect(tiles()).toHaveLength(0); // optimistic removal

		// Retry while the delete is still in flight.
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		failDelete(new Error('500'));
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('survivor.png');
		expect(toastMock).toHaveBeenCalledOnce();
		expect(String(toastMock.mock.calls[0][0])).toContain('survivor.png');
		expect(notifyDeletedMock).not.toHaveBeenCalled();
		confirmSpy.mockRestore();
	});

	it('suppresses a failed delete when the WORKSPACE changed under it', async () => {
		// `wsSlug` is reactive and the strip survives a workspace change, so
		// view identity is (workspace, item) — keying it on the item alone would
		// read that switch as a same-item Retry and roll A's row into B's strip
		// (Codex fresh-angle round 2).
		listMock.mockRejectedValue(new Error('offline'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();
		broadcastUpload('item-a', uploaded('survivor'));
		flushSync();

		let failDelete!: (err: Error) => void;
		deleteMock.mockReturnValue(
			new Promise<void>((_, reject) => {
				failDelete = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		flushSync();
		expect(tiles()).toHaveLength(0);

		// Retry clicked, THEN the workspace swapped before the effect flushed —
		// the window where an item-keyed retry marker is claimed by the wrong
		// view.
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		listMock.mockResolvedValue(response([att({ id: 'other-ws' })]));
		props.wsSlug = 'ws2';
		flushSync();
		await settle();

		failDelete(new Error('500'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('survivor.png'))).toBe(false);
		expect(names.some((n) => n.includes('other-ws.png'))).toBe(true);
		expect(toastMock).not.toHaveBeenCalled();
		confirmSpy.mockRestore();
	});

	it('still suppresses a failed delete after an A→B→A round trip', async () => {
		// The view generation, not just the id compare, is what closes this:
		// `itemId` reads 'item-a' again by the time the delete rejects, but the
		// row belongs to a view that has since been torn down and refetched —
		// restoring it would resurrect a row A's newer load didn't return
		// (PLAN-2105 / TASK-2112).
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		let failDelete!: (err: Error) => void;
		deleteMock.mockReturnValue(
			new Promise<void>((_, reject) => {
				failDelete = reject;
			})
		);
		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
		deleteButtons()[0].click();
		flushSync();

		listMock.mockResolvedValue(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		flushSync();
		await settle();

		listMock.mockResolvedValue(response([]));
		props.itemId = 'item-a';
		flushSync();
		await settle();

		failDelete(new Error('500'));
		await settle();

		expect(tiles()).toHaveLength(0);
		expect(toastMock).not.toHaveBeenCalled();
		confirmSpy.mockRestore();
	});

	// ── Upload refresh (TASK-2385) ────────────────────────────────────────

	const uploaded = (id: string): UploadedAttachment => ({
		id,
		filename: `${id}.png`,
		mime_type: 'image/png',
		size_bytes: 4096,
	});

	it('shows a dropped file immediately, without a refetch', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(1);

		broadcastUpload('item-a', uploaded('new1'));
		flushSync();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(2);
		expect(names[0]).toContain('new1.png'); // newest first
		expect(listMock).toHaveBeenCalledOnce(); // no refetch
	});

	it('renders the strip from empty when the first upload lands', async () => {
		// The strip renders nothing at all when empty, so this covers the
		// transition from no-element to mounted.
		listMock.mockResolvedValue(response([]));
		mountStrip('item-a');
		await settle();
		expect(target.querySelector('.attachment-strip')).toBeNull();

		broadcastUpload('item-a', uploaded('first'));
		flushSync();

		expect(target.querySelector('.attachment-strip')).not.toBeNull();
		expect(tiles()).toHaveLength(1);
	});

	it('survives an in-flight list() that predates the upload', async () => {
		// The GET was issued before the drop, so its response can't contain the
		// new row. Assigning it verbatim would erase the tile we just showed
		// (Codex review of TASK-2385).
		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValue(pending.promise);
		mountStrip('item-a');
		flushSync();

		broadcastUpload('item-a', uploaded('dropped'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		pending.resolve(response([att({ id: 'old1' })]));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(2);
		expect(names.some((n) => n.includes('dropped.png'))).toBe(true);
		expect(names.some((n) => n.includes('old1.png'))).toBe(true);
	});

	it('lets a later load retire a pending upload deleted outside this tab', async () => {
		// `pendingUploads` was retained forever and no successful response ever
		// consumed it, so it re-merged onto EVERY later load. The event bus is
		// process-local, so a deletion from another tab or another user, followed
		// by a load that legitimately returns no row, resurrected the tile — and
		// kept resurrecting it (final review round 4).
		//
		// The rule: a response is authoritative about the entries the buffer
		// already held when that request went OUT. Entries announced while it was
		// in flight are the buffer's actual purpose and stay.
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();
		expect(errorRow()).not.toBeNull();

		// Uploaded during the outage: buffered, and rightly shown.
		broadcastUpload('item-a', uploaded('elsewhere-deleted'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		// Retry. The GET goes out AFTER the upload, so the server's answer is
		// authoritative — and it says the row is gone, because another tab
		// deleted it.
		listMock.mockResolvedValueOnce(response([att({ id: 'still-here' })]));
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('elsewhere-deleted'))).toBe(false);
		expect(names.some((n) => n.includes('still-here.png'))).toBe(true);
	});

	it('does not retire a pending upload on a TRUNCATED page', async () => {
		// The request is bounded at 50 rows, so a FULL page is not proof of
		// absence — a still-live row can simply be past it. Retiring it there
		// would delete a good tile permanently, which is worse than the
		// resurrection the retirement rule exists to fix (Codex round 2).
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();

		broadcastUpload('item-a', uploaded('past-the-bound'));
		flushSync();

		// The retry's page comes back FULL, and the server says there are more.
		const full = Array.from({ length: 50 }, (_, i) => att({ id: `s${i}` }));
		listMock.mockResolvedValueOnce({ attachments: full, total: 60, limit: 50, offset: 0 });
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		target.querySelector<HTMLElement>('.att-more-expand')?.click();
		flushSync();
		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('past-the-bound.png'))).toBe(true);
	});

	it('treats a page that holds every live row as complete, even at the bound', async () => {
		// `total` is the server's own count and `offset` is always 0 here, so a
		// page of exactly MAX_FETCH rows with `total: 50` HAS reached everything.
		// Reading "full page" as "truncated" would leave an externally deleted
		// upload buffered — resurrectable indefinitely (Codex round 3).
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();

		broadcastUpload('item-a', uploaded('elsewhere-deleted'));
		flushSync();

		const full = Array.from({ length: 50 }, (_, i) => att({ id: `s${i}` }));
		listMock.mockResolvedValueOnce({ attachments: full, total: 50, limit: 50, offset: 0 });
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		target.querySelector<HTMLElement>('.att-more-expand')?.click();
		flushSync();
		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('elsewhere-deleted'))).toBe(false);
		expect(names).toHaveLength(50);
	});

	it('still keeps an upload announced while THIS request was in flight', async () => {
		// The other side of the same rule, and the reason the buffer exists at
		// all: this GET was issued BEFORE the upload, so its silence about the
		// row proves nothing and the tile must survive the response. Pins the
		// consumption above to entries the request could actually have covered.
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();

		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(pending.promise);
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();

		// Announced AFTER the retry's request went out.
		broadcastUpload('item-a', uploaded('mid-flight'));
		flushSync();

		pending.resolve(response([att({ id: 'server-row' })]));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('mid-flight.png'))).toBe(true);
		expect(names.some((n) => n.includes('server-row.png'))).toBe(true);
	});

	it('does not double-count an upload the refetch also returns', async () => {
		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValue(pending.promise);
		mountStrip('item-a');
		flushSync();

		broadcastUpload('item-a', uploaded('both'));
		flushSync();

		// The response DOES include it (the GET went out after all).
		pending.resolve(response([att({ id: 'both' })]));
		await settle();

		expect(tiles()).toHaveLength(1);
	});

	it('keeps an optimistic upload when the in-flight list() FAILS', async () => {
		// The upload succeeded; only the listing failed. Clearing the strip
		// would hide a row the editor and server both have (Codex round 2).
		let failList!: (err: Error) => void;
		listMock.mockReturnValue(
			new Promise<AttachmentListResponse>((_, reject) => {
				failList = reject;
			})
		);
		mountStrip('item-a');
		flushSync();

		broadcastUpload('item-a', uploaded('survivor'));
		flushSync();

		failList(new Error('offline'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(1);
		expect(names[0]).toContain('survivor.png');
		// ...and the failure is still stated alongside it (DR-10) — the tile
		// isn't a claim that the list loaded.
		expect(errorRow()).not.toBeNull();
	});

	it('ignores an upload announced for a DIFFERENT item', async () => {
		// Two panes can be open at once; a drop in the other one must not
		// appear here.
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();

		broadcastUpload('item-b', uploaded('elsewhere'));
		flushSync();

		expect(tiles()).toHaveLength(1);
	});

	it('does not duplicate an upload already present in the list', async () => {
		// The same id can arrive twice — a re-broadcast, or an upload that the
		// completed fetch already included.
		listMock.mockResolvedValue(response([att({ id: 'dupe' })]));
		mountStrip('item-a');
		await settle();

		broadcastUpload('item-a', uploaded('dupe'));
		flushSync();

		expect(tiles()).toHaveLength(1);
	});

	it('does not let a re-announced upload resurrect a deleted row', async () => {
		// The load path filters every response through the tombstones; the
		// upload path did not, so a delayed or duplicated upload event for a row
		// the user has since deleted put it straight back — into `attachments`
		// AND into `pendingUploads`, which then re-merges it onto every later
		// response (final review round 2). A confirmed deletion outranks a
		// repeat of an older upload event.
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();

		broadcastUpload('item-a', uploaded('late'));
		flushSync();
		expect(tiles()).toHaveLength(2);

		broadcastDeletion('late');
		flushSync();
		expect(tiles()).toHaveLength(1);

		// The bus re-announces the same upload after the deletion.
		broadcastUpload('item-a', uploaded('late'));
		flushSync();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(1);
		expect(names.some((n) => n.includes('late.png'))).toBe(false);
	});

	it('drops a tile when another surface broadcasts its deletion', async () => {
		// Settings → Storage, or the OTHER strip in a split pane. Both mount
		// concurrently, so a strip that only updated its own deletes would keep
		// showing a downloadable tile for a row that is gone (Codex round 17).
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(2);

		broadcastDeletion('a2');
		flushSync();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(1);
		expect(names[0]).toContain('a1.png');
	});

	it('does not let an in-flight fetch resurrect an already-deleted row', async () => {
		// The list request is still in flight when another surface announces a
		// deletion; the response must not paint the dead row back
		// (Codex round 18).
		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValue(pending.promise);
		mountStrip('item-a');
		flushSync();

		broadcastDeletion('a2');
		flushSync();

		pending.resolve(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(1);
		expect(names[0]).toContain('a1.png');
	});

	it('bounds the tombstone set, shedding the oldest ids first', async () => {
		// The tombstones survive a Retry and ride a workspace-wide bus, so the
		// set needs the same hard bound as the list buffers (final-review P3).
		// The newest tombstone is the one that can still be racing an in-flight
		// response, so the cap sheds from the other end.
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();

		broadcastDeletion('oldest');
		for (let i = 0; i < 500; i++) broadcastDeletion(`bulk-${i}`);
		broadcastDeletion('newest');
		flushSync();

		// Retry keeps the tombstones (an item switch would clear them).
		listMock.mockResolvedValueOnce(
			response([att({ id: 'oldest' }), att({ id: 'newest' }), att({ id: 'live' })])
		);
		target.querySelector<HTMLButtonElement>('.att-retry')?.click();
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('newest.png'))).toBe(false);
		expect(names.some((n) => n.includes('live.png'))).toBe(true);
		// Shed by the cap, so it is no longer suppressed — the safe direction.
		expect(names.some((n) => n.includes('oldest.png'))).toBe(true);
	});

	it('warns using UNFLUSHED editor content, not just the persisted body', async () => {
		// The image was inserted seconds ago: it's in the live editor markdown
		// but not yet in item.content. The warning must still fire.
		const uuid = '11111111-2222-4333-8444-555555555555';
		listMock.mockResolvedValue(response([att({ id: uuid })]));
		props.canDelete = true;
		props.itemContent = 'persisted body with no refs';
		props.liveContent = () => `just typed ![new](pad-attachment:${uuid})`;
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false);
		deleteButtons()[0].click();
		await settle();

		expect(String(confirmSpy.mock.calls[0][0])).toContain("still used in this item's content");
		confirmSpy.mockRestore();
	});

	it('falls back to persisted content when the live read throws', async () => {
		const uuid = '99999999-8888-4777-8666-555555555555';
		listMock.mockResolvedValue(response([att({ id: uuid })]));
		props.canDelete = true;
		props.itemContent = `persisted ![x](pad-attachment:${uuid})`;
		props.liveContent = () => {
			throw new Error('editor destroyed');
		};
		mountStrip('item-a');
		await settle();

		const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false);
		deleteButtons()[0].click();
		await settle();

		expect(String(confirmSpy.mock.calls[0][0])).toContain("still used in this item's content");
		confirmSpy.mockRestore();
	});
});
