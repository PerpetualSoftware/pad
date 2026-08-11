import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { AttachmentListItem, AttachmentListResponse } from '$lib/types';
import type { UploadedAttachment } from '$lib/attachments/events';
import { runTopEscape, _resetEscapeStackForTests } from '$lib/stores/escapeStack';
import { __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

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
// announceAttachmentDeleted bundles the notify + cache-invalidate pair; the mock
// below splits them back out so tests can assert each half.
const invalidateMock = vi.fn<(ws: string, uuid: string) => void>();
// TASK-2424 / T4a (TASK-2489): the strip is now an EMITTER on the UNIFIED SURFACE
// channel — both a file tile (`openOptions`) and an image tile (`openLightbox`)
// emit `notifyAttachmentSurfaceOpen`. Spied so the emission can be asserted as a
// payload, AND forwarded to the REAL bus so a mounted `AttachmentSurfaceHost` opens
// the real Lightbox end-to-end.
const surfaceOpenMock = vi.fn<(event: Record<string, unknown>) => void>();

// The bus stays REAL (importOriginal), so the strip's deletion subscription, the
// mounted host's surface + deletion subscriptions, and the Lightbox's own tombstone
// path all fan out for real. Only two exports are wrapped: `announceAttachmentDeleted`
// records the announce (notify + invalidate) for assertions WHILE calling the real
// `notifyAttachmentDeleted` so every live surface still hears it; and
// `notifyAttachmentSurfaceOpen` records the emit and forwards it to the real bus.
vi.mock('$lib/attachments/events', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/attachments/events')>();
	return {
		...actual,
		announceAttachmentDeleted: (ws: string, uuid: string) => {
			notifyDeletedMock(uuid);
			actual.notifyAttachmentDeleted(uuid);
			invalidateMock(ws, uuid);
		},
		notifyAttachmentSurfaceOpen: (event: unknown) => {
			surfaceOpenMock(event as Record<string, unknown>);
			return actual.notifyAttachmentSurfaceOpen(event as never);
		},
	};
});

// The mocked module — its `notifyAttachmentDeleted` / `notifyAttachmentUploaded`
// are the REAL ones (spread through), so a test can simulate ANOTHER surface's
// broadcast by calling them directly, exactly as the app would.
const events = await import('$lib/attachments/events');
function broadcastDeletion(uuid: string) {
	events.notifyAttachmentDeleted(uuid);
}
function broadcastUpload(itemId: string, a: UploadedAttachment) {
	events.notifyAttachmentUploaded(itemId, a);
}

// The strip also reaches the shared HEAD-metadata cache directly, on Retry
// (PLAN-2392 DR-10) — a separate spy from the events bus's, so the two can be
// asserted independently.
const invalidateMetadataMock = vi.fn<(ws: string, uuid: string) => void>();
// The strip mounts the real Lightbox, whose metadata header (TASK-2475) reads
// through this module — so the mock must carry the fetch/revalidate exports too,
// not just invalidate. Benign `ok` stubs: the strip's viewer tests use rows whose
// size is already known (no fetch fires), but a complete contract keeps the mount
// robust to any row shape.
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: () =>
		Promise.resolve({ status: 'ok' as const, mime: 'image/png', size: 2048 }),
	revalidateAttachmentMetadata: () =>
		Promise.resolve({ status: 'ok' as const, mime: 'image/png', size: 2048 }),
	invalidateAttachmentMetadata: (ws: string, uuid: string) => invalidateMetadataMock(ws, uuid),
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (message: string, kind?: string) => toastMock(message, kind) },
}));

const { default: ItemAttachmentStrip } = await import('./ItemAttachmentStrip.svelte');
// T4a (TASK-2489): the strip no longer mounts `Lightbox` itself — it EMITS on the
// surface channel and the ONE `AttachmentSurfaceHost` owns the mount. The viewer
// tests mount that host, addressed to the strip's (itemId, hostToken), so a tile
// activation renders the real Lightbox end-to-end.
const { default: AttachmentSurfaceHost } = await import(
	'$lib/components/attachments/AttachmentSurfaceHost.svelte'
);

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
	parentArchived: boolean;
	mutationsEnabled?: boolean;
	getItemContent?: () => string | null;
	getLiveContent?: () => string | null;
}>({
	wsSlug: 'ws',
	username: 'dave',
	itemId: null,
	canDelete: false,
	itemContent: null,
	liveContent: null,
	hostToken: 'host-1',
	parentArchived: false,
	mutationsEnabled: false,
	getItemContent: undefined,
	getLiveContent: undefined,
});

describe('ItemAttachmentStrip', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;
	let hostTarget: HTMLElement;
	let hostApp: ReturnType<typeof mount> | undefined;

	beforeEach(() => {
		listMock.mockReset();
		deleteMock.mockReset();
		deleteMock.mockResolvedValue(undefined);
		toastMock.mockReset();
		notifyDeletedMock.mockReset();
		invalidateMock.mockReset();
		invalidateMetadataMock.mockReset();
		surfaceOpenMock.mockReset();
		props.hostToken = 'host-1';
		props.wsSlug = 'ws';
		props.username = 'dave';
		props.itemId = null;
		props.canDelete = false;
		props.itemContent = null;
		props.liveContent = null;
		props.parentArchived = false;
		props.mutationsEnabled = false;
		props.getItemContent = undefined;
		props.getLiveContent = undefined;
		target = document.body.appendChild(document.createElement('div'));
		hostTarget = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		// The host owns the viewer now (T4a); tear it down first so the Lightbox's
		// own teardown unregisters its Escape handler and releases its backdrop lease.
		if (hostApp) unmount(hostApp);
		hostApp = undefined;
		if (instance) unmount(instance);
		instance = undefined;
		target.remove();
		hostTarget.remove();
		// The viewer registers on the shared ESC stack (TASK-2429) and the backdrop
		// lease registry; a case that leaves one open would otherwise leak into the
		// next test.
		_resetEscapeStackForTests();
		__resetViewerBackdropForTests();
	});

	/**
	 * Mount the ONE `AttachmentSurfaceHost` the viewer tests rely on. It reads the
	 * SAME reactive `props` (wsSlug / itemId / hostToken / mutationsEnabled / the
	 * content getters); its extra fields are ignored and `resourceGen` /
	 * `parentArchived` default to their inert values. Call it AFTER `mountStrip`
	 * so the address (itemId) is set, and the strip's surface emit reaches it.
	 */
	function mountViewerHost() {
		hostApp = mount(AttachmentSurfaceHost, { target: hostTarget, props });
		flushSync();
	}

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

	it('emits the open-surface event with the invoker and all three metadata fields', async () => {
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

		expect(surfaceOpenMock).toHaveBeenCalledTimes(1);
		// A file tile opens the unified surface as a SINGLE-attachment open (T4a):
		// `workspaceSlug` captured, `itemId`/`hostToken` route it (DR-8), the tile is
		// the `invoker` (focus-return), a single-image set DESCRIBES the file, and the
		// flat seeds equal images[0]. The strip always has all three from its list row.
		expect(surfaceOpenMock).toHaveBeenCalledWith({
			attachmentId: 'doc',
			workspaceSlug: 'ws',
			itemId: 'item-a',
			hostToken: 'host-1',
			images: [
				{
					id: 'doc',
					alt: 'spec.pdf',
					filename: 'spec.pdf',
					mime_type: 'application/pdf',
					size_bytes: 1536,
					width: null,
					height: null,
				},
			],
			index: 0,
			invoker: tile,
			filename: 'spec.pdf',
			mime_type: 'application/pdf',
			size_bytes: 1536,
		});
	});

	it('opens a BLANK-filename file tile — flat seed normalized to match images[0] (T4a)', async () => {
		// Regression: a blank filename is `''` on the row but `null` on the image
		// record; if the flat seed stayed `''` the notify validator would reject the
		// mismatch and the file would silently fail to open. Both are normalized to
		// null, so the emit is accepted and the surface opens.
		listMock.mockResolvedValue(
			response([att({ id: 'doc', mime_type: 'application/pdf', filename: '', size_bytes: 1536 })])
		);
		mountStrip('item-a');
		await settle();

		tiles()[0].click();
		flushSync();

		expect(surfaceOpenMock).toHaveBeenCalledTimes(1);
		const event = surfaceOpenMock.mock.calls[0][0] as Record<string, unknown>;
		// The flat seed EQUALS images[0] (both null), so the notify validator accepts
		// it — a `''` flat seed against a `null` record would have been dropped.
		expect((event.images as Array<Record<string, unknown>>)[0].filename).toBeNull();
		expect(event.filename).toBeNull();
	});

	it('adds no keydown handler that would race the UA activation click', async () => {
		// NAMED for what it can prove. "Activates exactly once per key press" is
		// the requirement (DR-12), but jsdom does not synthesise a button's
		// activation click, so no jsdom test can demonstrate it — the browser
		// suite does (web/e2e/item-attachment-strip.spec.ts). What IS falsifiable
		// here is the failure mode DR-12 names: a hand-rolled keydown handler
		// firing ALONGSIDE the UA's click and opening the panel twice.
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
			expect(surfaceOpenMock).not.toHaveBeenCalled();
			// ...and Space must not be swallowed as a scroll suppressor either:
			// the UA's own default handling is what we are relying on.
			expect(event.defaultPrevented).toBe(false);
		}
		// The UA's click is then the one and only activation path.
		tile.click();
		flushSync();
		expect(surfaceOpenMock).toHaveBeenCalledTimes(1);
	});

	it('renders an SVG as a FILE tile; the surface draws it on the fallback arm, not as raster bytes (DR-16)', async () => {
		// `isImage` would say yes to an SVG — the RASTER set takes an exact allowlist
		// instead, because SVG can carry active content. Post-convergence (T4a) the SVG
		// still opens the unified surface (as a file), but on the NO-BYTES fallback arm:
		// the DR-16 guarantee moved from "no viewer at all" to "no raster <img>",
		// enforced by the surface's own renderer downstream. At the producer level the
		// SVG is one `notifyAttachmentSurfaceOpen`, exactly like the PNG.
		listMock.mockResolvedValue(
			response([
				att({ id: 'svg', mime_type: 'image/svg+xml', filename: 'logo.svg' }),
				att({ id: 'png', mime_type: 'image/png', filename: 'shot.png' }),
			])
		);
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		const [svgTile, pngTile] = tiles();
		// The SVG got the file path: an icon + name, no thumbnail request.
		expect(svgTile.querySelector('img')).toBeNull();
		expect(svgTile.getAttribute('aria-label')).toContain('Options for logo.svg');

		svgTile.click();
		await settle();
		expect(surfaceOpenMock).toHaveBeenCalledTimes(1);
		// Opened — but on the FALLBACK arm: a real surface on screen, no raster bytes
		// for the SVG.
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();
		expect(document.querySelector('.lightbox-fallback')).not.toBeNull();
		expect(document.querySelector('.lightbox-image')).toBeNull();

		// ...and the SVG isn't a member of the PNG's RASTER set either, so the PNG's
		// surface holds ONE image rather than paging into the SVG (the counter only
		// renders for a multi-image set, so its absence IS the assertion).
		surfaceOpenMock.mockClear();
		pngTile.click();
		await settle();
		expect(surfaceOpenMock).toHaveBeenCalledTimes(1);
		expect(document.querySelector('.lightbox-image')).not.toBeNull();
		expect(document.querySelector('.lightbox-counter')).toBeNull();
	});

	it('renders images as thumb-sm buttons that open the lightbox', async () => {
		listMock.mockResolvedValue(response([att({ id: 'img1' }), att({ id: 'img2' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		const tile = tiles()[1];
		expect(tile.tagName).toBe('BUTTON');
		expect(tile.querySelector('img')?.getAttribute('src')).toBe(
			'/api/v1/workspaces/ws/attachments/img2?variant=thumb-sm'
		);

		tile.click();
		await settle();
		// The lightbox opens on the clicked image, with both images available
		// so ←/→ page through the item's attachments.
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('2 / 2');
	});

	it('opens a FRESH viewer when a second tile is activated (TASK-2431)', async () => {
		// The viewer seeds its index — and its own MIME filter — once at mount,
		// so the host's mount must be keyed per open. Without that, a second open
		// reuses the instance and keeps showing the first image.
		//
		// In the app an open viewer inerts the tiles behind it, so this is
		// insurance rather than a live path; jsdom does not implement inertness,
		// which is what makes the invariant testable at all.
		listMock.mockResolvedValue(response([att({ id: 'img1' }), att({ id: 'img2' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		tiles()[0].click();
		await settle();
		expect(
			document.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt')
		).toBe('img1.png');

		tiles()[1].click();
		await settle();
		expect(
			document.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt')
		).toBe('img2.png');
		// ...and exactly one viewer, not two stacked.
		expect(document.querySelectorAll('.lightbox-backdrop')).toHaveLength(1);
	});

	it('opens the lightbox at the IMAGE index, not the attachment index', async () => {
		// Interleaved non-images: a naive `attachments.indexOf(att)` would open
		// the wrong image, since the raster set only ever receives image rows.
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
		mountViewerHost();

		// Fourth tile overall, but the SECOND image.
		tiles()[3].click();
		await settle();
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

		// Escape is no longer a local `window` listener on the viewer (TASK-2429):
		// the shared `escapeStack` is its ONE owner, driven by the route host. So
		// the close is exercised the way the app reaches it — through the stack —
		// and a raw keydown is asserted to do NOTHING, which is the regression
		// that would reintroduce the two-owners-one-press bug.
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		flushSync();
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();

		expect(runTopEscape()).toBe(true);
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


	// ── Delete (TASK-2384, confirmation reworked in TASK-2425) ────────────
	//
	// The affordance is gated on ItemDetail's `mutationsEnabled`
	// (canEdit && !peeking) per PLAN-2382 DR-6, and the confirm text has to
	// stay honest about what was actually checked (DR-5): "referenced in this
	// item's content" is knowable client-side; "unused anywhere" is not.
	//
	// TASK-2425 (PLAN-2392 DR-18) replaced the browser-native `window.confirm`
	// these tests used to spy on with the SAME in-app drill-down the options
	// panel shows — so they now drive the real rows. That is not a cosmetic
	// change to the tests: the native confirm blocked the thread, so nothing
	// could move between the entry fence and the request, while the in-app one
	// leaves a window in which the user can switch item or workspace. Every
	// fence and rollback assertion below is preserved, and the confirmation is
	// driven through the rows a user would actually click.

	function deleteButtons(): HTMLButtonElement[] {
		return Array.from(target.querySelectorAll<HTMLButtonElement>('.att-delete'));
	}

	/** Portaled to <body> like every other Menu, so queried document-wide. */
	function confirmPanel(): HTMLElement | null {
		return document.querySelector<HTMLElement>('[role="menu"]');
	}

	function confirmRows(): HTMLElement[] {
		return Array.from(document.querySelectorAll<HTMLElement>('[role="menu"] [role="menuitem"]'));
	}

	/** By VISIBLE label — MenuItem's icon span is part of `textContent`. */
	function confirmRow(label: string): HTMLElement | undefined {
		return confirmRows().find(
			(el) => el.querySelector('.mi-label')?.textContent?.trim() === label
		);
	}

	function promptText(): string {
		return document.querySelector('.attachment-delete-prompt')?.textContent ?? '';
	}

	/** Click a tile's `×`. Opens the confirmation; sends nothing. */
	function openConfirm(index = 0) {
		deleteButtons()[index].click();
		flushSync();
	}

	/** The destructive row — the only thing that issues a DELETE. */
	function clickConfirm() {
		confirmRow('Delete file')!.click();
		flushSync();
	}

	function clickCancel() {
		confirmRow('Cancel')!.click();
		flushSync();
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

	it('confirms in-app, never with a browser dialog, Cancel first (DR-18)', async () => {
		// The shape the item menu establishes and the options panel already
		// used: prompt as `role="presentation"` (a role="menu" owns only
		// menuitem / separator / group children), an aria-describedby
		// back-reference from the destructive row so the otherwise-unannounced
		// prompt is read out, Cancel FIRST so the menu's focus handoff can
		// never land Enter on Delete.
		const nativeConfirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();

		expect(nativeConfirm).not.toHaveBeenCalled();
		const prompt = document.querySelector('.attachment-delete-prompt');
		expect(prompt?.getAttribute('role')).toBe('presentation');
		const rows = confirmRows();
		const labelOf = (el: HTMLElement) => el.querySelector('.mi-label')?.textContent?.trim();
		expect(labelOf(rows[0])).toBe('Cancel');
		expect(labelOf(rows[rows.length - 1])).toBe('Delete file');
		expect(rows[rows.length - 1].getAttribute('aria-describedby')).toBe(prompt?.id);
		// Opening the confirmation is not a delete.
		expect(deleteMock).not.toHaveBeenCalled();
		nativeConfirm.mockRestore();
	});

	it('cancelling sends nothing, keeps the tile, and refocuses the × control', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		const closeBtn = deleteButtons()[0];
		openConfirm();
		clickCancel();
		await settle();

		expect(deleteMock).not.toHaveBeenCalled();
		expect(tiles()).toHaveLength(1);
		expect(confirmPanel()).toBeNull();
		// `window.confirm` restored focus for free. The control is also
		// opacity-hidden unless its cell has focus-within, so dropping focus to
		// <body> would make the affordance vanish under a keyboard user.
		expect(document.activeElement).toBe(closeBtn);
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

		openConfirm();

		expect(promptText()).toContain("still used in this item's content");
		// Declined → nothing deleted, tile stays.
		clickCancel();
		await settle();
		expect(deleteMock).not.toHaveBeenCalled();
		expect(tiles()).toHaveLength(1);
	});

	it('never claims an unreferenced attachment is unused', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		props.canDelete = true;
		props.itemContent = 'no references here';
		mountStrip('item-a');
		await settle();

		openConfirm();

		const message = promptText();
		// Comment bodies and other items are NOT scanned client-side (DR-5),
		// so the copy must hedge rather than assert non-use.
		expect(message).toContain('may still be referenced');
		expect(message).not.toContain('not used');
	});

	it('removes the tile optimistically and calls the API on confirm', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();
		clickConfirm();
		await settle();

		expect(deleteMock).toHaveBeenCalledWith('ws', 'a1');
		expect(tiles()).toHaveLength(1);
		expect(toastMock).not.toHaveBeenCalled();
		// The confirmation goes with the row it was asking about.
		expect(confirmPanel()).toBeNull();
		// An <img> already painted in the editor never re-requests, so the
		// NodeView has to be told or the body keeps showing a deleted image
		// until reload (Codex round 12).
		expect(notifyDeletedMock).toHaveBeenCalledWith('a1');
		expect(invalidateMock).toHaveBeenCalledWith('ws', 'a1');
	});

	it('abandons an open confirmation when the item switches under it', async () => {
		// The in-app confirmation does NOT block the thread the way
		// `window.confirm` did, so this window exists at all only as of
		// TASK-2425: the prompt can still be up when the strip repaints for a
		// different item. Leaving it there would delete the PREVIOUS item's
		// attachment from behind the new one.
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();
		expect(confirmPanel()).not.toBeNull();

		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		flushSync();
		await settle();

		expect(confirmPanel()).toBeNull();
		expect(deleteMock).not.toHaveBeenCalled();
	});

	it('drops an open confirmation when another surface deletes that row', async () => {
		// The tile the confirmation is anchored to has just been unmounted, so
		// the menu would be left pointing at a detached element — and the
		// question it is asking has already been answered.
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();
		expect(confirmPanel()).not.toBeNull();

		broadcastDeletion('a1');
		flushSync();

		expect(confirmPanel()).toBeNull();
		expect(deleteMock).not.toHaveBeenCalled();
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
		props.itemId = 'item-b';
		// No flushSync between the switch and the click: that IS the window.
		deleteButtons()[0].click();
		await settle();

		// Not even prompted — the tile the user aimed at no longer exists.
		expect(confirmPanel()).toBeNull();
		expect(deleteMock).not.toHaveBeenCalled();
		expect(notifyDeletedMock).not.toHaveBeenCalled();
		expect(tiles()[0].getAttribute('aria-label')).toContain('b1.png');
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
		props.wsSlug = 'ws2';
		deleteButtons()[0].click();
		await settle();

		expect(confirmPanel()).toBeNull();
		expect(deleteMock).not.toHaveBeenCalled();
		expect(tiles()[0].getAttribute('aria-label')).toContain('ws2-row.png');
	});

	it('refuses a CONFIRMATION that lands after the item already switched', async () => {
		// The window `window.confirm` did not have: it blocked the thread, so
		// the entry fence taken when the `×` was clicked was still true by
		// definition when it returned. An in-app confirmation can sit on screen
		// across a switch, so the fence is re-checked at the point that
		// actually sends the request (TASK-2425).
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();

		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		// No flushSync: the prompt is still up and the props already read B.
		clickConfirm();
		await settle();

		expect(deleteMock).not.toHaveBeenCalled();
		expect(notifyDeletedMock).not.toHaveBeenCalled();
		expect(tiles()[0].getAttribute('aria-label')).toContain('b1.png');
	});

	it('rolls the tile back and toasts when the delete fails', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		deleteMock.mockRejectedValue(new Error('403'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();
		clickConfirm();
		await settle();

		expect(tiles()).toHaveLength(2);
		// Nothing was deleted server-side, so the editor must NOT be told.
		expect(notifyDeletedMock).not.toHaveBeenCalled();
		expect(toastMock).toHaveBeenCalledOnce();
		expect(String(toastMock.mock.calls[0][0])).toContain('a1.png');
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
		openConfirm();
		clickConfirm();
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
		openConfirm(); // a1 — in flight, will fail
		clickConfirm();
		openConfirm(); // now b1 — resolves immediately
		clickConfirm();
		await settle();

		failFirst(new Error('boom'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(true); // restored
		expect(names.some((n) => n.includes('b1.png'))).toBe(false); // stays deleted
		// ...and restored at its original position, not appended.
		expect(names[0]).toContain('a1.png');
	});

	it('still announces the deletion when the delete 404s', async () => {
		// A 404 proves the row is gone just as well as a 204 does, so the other
		// local surfaces need telling either way (Codex round 19).
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		deleteMock.mockRejectedValue(new FakeApiError('not_found'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();
		clickConfirm();
		await settle();

		expect(notifyDeletedMock).toHaveBeenCalledWith('a1');
		expect(invalidateMock).toHaveBeenCalledWith('ws', 'a1');
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
		openConfirm();
		clickConfirm();

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
		openConfirm();
		clickConfirm();

		broadcastDeletion('a1');
		flushSync();

		failDelete(new Error('500'));
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(false);
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

		openConfirm();
		clickConfirm();
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(toastMock).not.toHaveBeenCalled();
	});

	it('names permission as the reason on a 403, and restores the tile', async () => {
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		deleteMock.mockRejectedValue(new FakeApiError('forbidden'));
		props.canDelete = true;
		mountStrip('item-a');
		await settle();

		openConfirm();
		clickConfirm();
		await settle();

		expect(tiles()).toHaveLength(1);
		expect(String(toastMock.mock.calls[0][0])).toContain("don't have permission");
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
		openConfirm();
		clickConfirm();
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
		openConfirm();
		clickConfirm();
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
		openConfirm();
		clickConfirm();

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
	});

	// ── Upload refresh (TASK-2385) ────────────────────────────────────────

	const uploaded = (id: string): UploadedAttachment => ({
		id,
		filename: `${id}.png`,
		mime_type: 'image/png',
		size_bytes: 4096,
		width: null,
		height: null,
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

	it('an upload during an open surface joins the STRIP but NOT the open set (PLAN-2392 3c-iii U4 / TASK-2513)', async () => {
		// The open-set mutation contract, pinned end-to-end (DR-15 style) rather
		// than assumed. An upload that lands while the surface is open must NOT
		// retro-join its set, while the strip's own tile list — which subscribes to
		// the upload bus — MUST gain the row. What THIS test pins is the NO-LIVE-FOLLOW
		// half: the plausible wrong design is a host that synced new uploads into the
		// open request, and the surface leg falsifies exactly that. (The other half of
		// the snapshot contract — a producer mutating its OWN array/records in place
		// after emit can't reach the surface — is the deep copy in
		// notifyAttachmentSurfaceOpen, pinned directly at the module level by
		// events.test.ts's deep-snapshot test.) The two legs here are independent: a
		// dead upload bus fails the strip leg while leaving the surface leg green, and
		// a live-following surface fails the surface leg while leaving the strip leg
		// green — neither mechanism can mask the other.
		listMock.mockResolvedValue(response([att({ id: 'img1' }), att({ id: 'img2' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		// Open the surface on the strip's 2-image set, at image 1 of 2. The counter
		// reads `position / total`, so its DENOMINATOR is the set size.
		tiles()[0].click();
		await settle();
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');

		// A THIRD image is uploaded while the surface is open. It is viewer-eligible
		// (image/png), so a live-following surface would grow the denominator to
		// "1 / 3" — which is exactly the wrong behavior this asserts against.
		broadcastUpload('item-a', uploaded('img3'));
		flushSync();

		// Surface set UNCHANGED: still a 2-member set, still paging the emit-time
		// snapshot.
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
		expect(
			document.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt')
		).toBe('img1.png');

		// Strip DID gain the upload — its own tile list follows the bus.
		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(3);
		expect(names.some((n) => n.includes('img3.png'))).toBe(true);
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

		openConfirm();

		expect(promptText()).toContain("still used in this item's content");
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

		openConfirm();

		expect(promptText()).toContain("still used in this item's content");
	});

	// ── Viewer toolbar (TASK-2474) ───────────────────────────────────────────
	// The STRIP origin of the three that mount a Lightbox. These prove the strip
	// forwards its `mutationsEnabled` to the viewer's toolbar (separate from the
	// tile-delete `canDelete` above).
	function toolbarLabels(): string[] {
		return Array.from(
			document.querySelectorAll<HTMLElement>('.lightbox-toolbar .lightbox-tool')
		).map((t) => t.getAttribute('aria-label') ?? '');
	}

	it('opens a viewer toolbar with Delete when the strip is granted mutations', async () => {
		props.mutationsEnabled = true;
		listMock.mockResolvedValue(response([att({ id: 'img1' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		tiles()[0].click();
		await settle();
		expect(document.querySelector('.lightbox-toolbar')).not.toBeNull();
		expect(toolbarLabels()).toContain('Delete');
	});

	it('opens a read-only viewer toolbar (no Delete) when mutations are withheld', async () => {
		props.mutationsEnabled = false;
		listMock.mockResolvedValue(response([att({ id: 'img1' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		tiles()[0].click();
		await settle();
		expect(document.querySelector('.lightbox-toolbar')).not.toBeNull();
		expect(toolbarLabels()).toContain('Download');
		expect(toolbarLabels()).not.toContain('Delete');
	});

	it('confirming Delete in the toolbar deletes the attachment and closes the viewer', async () => {
		// The full path: the descriptor owns the delete (api + announce), the module
		// owns the gate, and a confirmed delete closes the viewer over the now-gone
		// image. The same delete call the tile's × makes — one confirmation, one API.
		props.mutationsEnabled = true;
		listMock.mockResolvedValue(response([att({ id: 'img1' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		tiles()[0].click();
		await settle();
		document.querySelector<HTMLButtonElement>(
			'.lightbox-toolbar .lightbox-tool[aria-label="Delete"]'
		)!.click();
		flushSync();
		// Drill-down up; confirm it.
		const confirmRow = Array.from(
			document.querySelectorAll<HTMLButtonElement>('.lightbox-delete-confirm button')
		).find((b) => b.textContent?.includes('Delete file'))!;
		expect(confirmRow).toBeDefined();
		confirmRow.click();
		await settle();

		expect(deleteMock).toHaveBeenCalledWith('ws', 'img1');
		expect(notifyDeletedMock).toHaveBeenCalledWith('img1');
		// The viewer closed over the deleted image.
		expect(document.querySelector('.lightbox-backdrop')).toBeNull();
	});

	it('confirming Delete on ONE of TWO images ADVANCES the viewer, not closes it (TASK-2477)', async () => {
		// The full toolbar → confirm → api.delete → announce → deletion-bus →
		// survivor-advance path, with MORE THAN ONE image: the viewer must ADVANCE to
		// the survivor rather than close (the retired C1 close-on-delete latch).
		props.mutationsEnabled = true;
		listMock.mockResolvedValue(response([att({ id: 'img1' }), att({ id: 'img2' })]));
		mountStrip('item-a');
		await settle();
		mountViewerHost();

		tiles()[0].click(); // open the viewer on img1
		await settle();
		expect(document.querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');

		document.querySelector<HTMLButtonElement>(
			'.lightbox-toolbar .lightbox-tool[aria-label="Delete"]'
		)!.click();
		flushSync();
		Array.from(document.querySelectorAll<HTMLButtonElement>('.lightbox-delete-confirm button'))
			.find((b) => b.textContent?.includes('Delete file'))!
			.click();
		await settle();

		expect(deleteMock).toHaveBeenCalledWith('ws', 'img1');
		// The viewer stays OPEN, advanced to the surviving img2.
		expect(document.querySelector('.lightbox-backdrop')).not.toBeNull();
		expect(document.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt')).toBe(
			'img2.png'
		);
	});

	// ── Archive / restore revalidation (PLAN-2392 3c-iii U2 / TASK-2511) ──────
	//
	// The strip has NO SSE subscription: its only live inputs are the in-process
	// delete/upload buses, so a restore that happened while this browser was
	// elsewhere never reaches it and its rows keep rendering from the pre-archive
	// fetch. `parentArchived` — threaded from ItemDetail, the same signal the
	// timeline and surface host take — is the missing edge. Restore (true→false)
	// re-fetches and MERGES over the current rows WITHOUT blanking, preserving
	// tombstones / pending uploads / in-flight deletes / expanded state. Archive
	// (false→true) is a content no-op. The latch is keyed to VIEW IDENTITY, so an
	// archived→active item SWITCH is not misread as a restore.

	it('treats a mount on an already-archived item as a level, not a restore edge', async () => {
		// The latch is seeded from the initial prop, so an already-archived mount
		// is a LEVEL — one fetch (the mount load), no spurious restore refetch.
		props.parentArchived = true;
		listMock.mockResolvedValue(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();

		expect(listMock).toHaveBeenCalledTimes(1);
		expect(tiles()).toHaveLength(1);
	});

	it('revalidates on restore (archived true→false), merging fresh rows without blanking', async () => {
		// Mount already-archived: the tiles are the pre-archive snapshot.
		props.parentArchived = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(1);
		expect(listMock).toHaveBeenCalledTimes(1);

		// The restore refetch is DEFERRED so the pre-restore tile can be observed
		// still on screen while it is in flight — the gentle path never blanks.
		const revalidate = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(revalidate.promise);
		props.parentArchived = false;
		flushSync();
		expect(listMock).toHaveBeenCalledTimes(2);
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('a1.png');

		// The response carries an attachment that appeared while archived.
		revalidate.resolve(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		await settle();
		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names).toHaveLength(2);
		expect(names.some((n) => n.includes('a2.png'))).toBe(true);
	});

	it('does not refetch or blank on archive (false→true) — the content no-op contract', async () => {
		props.parentArchived = false;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(2);
		expect(listMock).toHaveBeenCalledTimes(1);

		props.parentArchived = true;
		flushSync();
		await settle();

		// Tiles keep their painted bytes; the archive edge issues no list() at all.
		expect(tiles()).toHaveLength(2);
		expect(listMock).toHaveBeenCalledTimes(1);
	});

	it('does not double-load when an archived item switches to an active one', async () => {
		// A(archived) → B(active) is a true→false transition in the raw prop, but a
		// SWITCH, not a restore (round-3 P2). The switch's own load is the ONLY
		// fetch; a value-only latch would fire a duplicate restore revalidation on
		// top of it. The identity-keyed latch reseeds to B instead.
		props.parentArchived = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();
		expect(listMock).toHaveBeenCalledTimes(1);

		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		props.parentArchived = false;
		flushSync();
		await settle();

		// Exactly TWO fetches: mount-A and switch-B. A third is the bug.
		expect(listMock).toHaveBeenCalledTimes(2);
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('b1.png');
	});

	it('does not repaint an in-flight-deleted row when a restore revalidation lands first', async () => {
		// round-3 P1: `deletedIds` is only tombstoned AFTER the delete's await, so
		// a restore refetch in the optimistic-removal window would otherwise
		// repaint the just-removed tile. The in-flight-delete marker excludes it.
		props.parentArchived = true;
		props.canDelete = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(2);

		// Delete a1 with a DEFERRED delete, so its tombstone broadcast hasn't
		// fired when the restore refetch resolves.
		const del = deferred<void>();
		deleteMock.mockReturnValueOnce(del.promise);
		openConfirm(0); // a1 is the first tile's ×
		clickConfirm();
		expect(tiles()).toHaveLength(1); // optimistic removal of a1

		// Restore. The refetch still lists a1 — its own DELETE hasn't committed —
		// but a1 must NOT come back.
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		let names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(false);
		expect(names.some((n) => n.includes('a2.png'))).toBe(true);

		// The delete now resolves successfully: still gone, now tombstoned too.
		del.resolve();
		await settle();
		names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1.png'))).toBe(false);
		expect(deleteMock).toHaveBeenCalledWith('ws', 'a1');
	});

	it('preserves a pending upload through a restore revalidation', async () => {
		props.parentArchived = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();

		// A drop announced while archived — in `attachments` AND `pendingUploads`.
		broadcastUpload('item-a', uploaded('fresh'));
		flushSync();
		expect(tiles()).toHaveLength(2);

		// The restore refetch does NOT return `fresh` (the server doesn't know it
		// yet). The gentle path must merge it back on top, not drop it.
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('fresh.png'))).toBe(true);
		expect(names.some((n) => n.includes('a1.png'))).toBe(true);
	});

	it('does not resurrect a tombstoned row on a restore revalidation', async () => {
		props.parentArchived = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();

		// Another surface deleted a2 while archived.
		broadcastDeletion('a2');
		flushSync();
		expect(tiles()).toHaveLength(1);

		// The restore refetch still lists a2 (its delete raced the server, or this
		// GET predates it). The tombstone must win.
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a2.png'))).toBe(false);
		expect(names).toHaveLength(1);
	});

	it('preserves the expanded state across a restore revalidation', async () => {
		props.parentArchived = true;
		const rows = Array.from({ length: 12 }, (_, i) => att({ id: `a${i}` }));
		listMock.mockResolvedValueOnce(response(rows));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(8);

		// Expand — the non-retry load path would reset this to false.
		target.querySelector<HTMLElement>('.att-more-expand')?.click();
		flushSync();
		expect(tiles()).toHaveLength(12);

		listMock.mockResolvedValueOnce(response(rows));
		props.parentArchived = false;
		flushSync();
		await settle();

		// Still expanded: the gentle path never touched `expanded`.
		expect(tiles()).toHaveLength(12);
		expect(target.querySelector('.att-more-expand')).toBeNull();
	});

	it('keeps the pre-restore rows when the restore revalidation fails', async () => {
		props.parentArchived = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(1);

		listMock.mockRejectedValueOnce(new Error('offline'));
		props.parentArchived = false;
		flushSync();
		await settle();

		// A refresh failure is not authoritative: keep the good rows, and do NOT
		// degrade to the blocking error row.
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('a1.png');
		expect(errorRow()).toBeNull();
	});

	it('clears a stale load error when a restore revalidation succeeds', async () => {
		// The initial load failed, leaving the error row up. A restore then fires a
		// SUCCESSFUL revalidation, which must clear the stale error — otherwise the
		// restored tiles render UNDER a lingering "Couldn't load" row.
		props.parentArchived = true;
		listMock.mockRejectedValueOnce(new Error('offline'));
		mountStrip('item-a');
		await settle();
		expect(errorRow()).not.toBeNull();

		listMock.mockResolvedValueOnce(response([att({ id: 'a1' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		expect(errorRow()).toBeNull();
		expect(tiles()).toHaveLength(1);
	});

	it('defers to a load still in flight instead of superseding it (no strand on failure)', async () => {
		// A restore that superseded a still-pending mount load would strand the
		// strip empty with no error/retry if the revalidation then failed (Codex
		// round 2). The revalidation instead DEFERS: the in-flight load already
		// returns the current list (archive doesn't delete attachments), so no
		// second fetch is issued, and that load remains responsible for the outcome.
		props.parentArchived = true;
		const pending = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(pending.promise);
		mountStrip('item-a');
		flushSync();
		expect(listMock).toHaveBeenCalledTimes(1);

		// Restore while the mount load is still in flight: no new fetch.
		props.parentArchived = false;
		flushSync();
		await settle();
		expect(listMock).toHaveBeenCalledTimes(1);

		// The original load completes and paints; nothing was stranded.
		pending.resolve(response([att({ id: 'a1' })]));
		await settle();
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('a1.png');
	});

	it('is not suppressed by a lingering stale prior-view load (per-view defer)', async () => {
		// The defer guard is keyed PER VIEW. The api client has no request abort, so
		// item A's load lingers in flight after a switch to B; a GLOBAL guard would
		// let that stale A-load suppress B's genuine restore revalidation, leaving B
		// stale with nothing to refetch it (Codex round 3).
		props.parentArchived = true;
		const aPending = deferred<AttachmentListResponse>(); // A's load never resolves
		listMock.mockReturnValueOnce(aPending.promise);
		mountStrip('item-a');
		flushSync();
		expect(listMock).toHaveBeenCalledTimes(1);

		// Switch to B (still archived): B's load resolves; A's is still in flight.
		listMock.mockResolvedValueOnce(response([att({ id: 'b1' })]));
		props.itemId = 'item-b';
		flushSync();
		await settle();
		expect(listMock).toHaveBeenCalledTimes(2);
		expect(tiles()).toHaveLength(1);

		// Restore B. B's own load is done; only A's stale load lingers. The restore
		// revalidation must still run (per-view key), not defer to A's load.
		listMock.mockResolvedValueOnce(response([att({ id: 'b1' }), att({ id: 'b2' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		expect(listMock).toHaveBeenCalledTimes(3);
		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('b2.png'))).toBe(true);
	});

	it('recomputes the continuation count from the restore response', async () => {
		props.parentArchived = true;
		const rows = Array.from({ length: 50 }, (_, i) => att({ id: `a${i}` }));
		listMock.mockResolvedValueOnce({ attachments: rows, total: 120, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();
		expect(target.querySelector('a.att-more-link')?.textContent?.trim()).toBe('View all (120)');

		// While archived, some were deleted elsewhere: the restore's total is lower.
		// The continuation must track the fresh response, not stay frozen at 120.
		listMock.mockResolvedValueOnce({ attachments: rows, total: 80, limit: 50, offset: 0 });
		props.parentArchived = false;
		flushSync();
		await settle();

		expect(target.querySelector('a.att-more-link')?.textContent?.trim()).toBe('View all (80)');
	});

	it('keeps an open delete confirmation across a restore revalidation', async () => {
		// The non-retry load path nulls `pendingDelete`; the gentle restore path
		// must not — the confirmation is anchored to a row that still exists.
		props.parentArchived = true;
		props.canDelete = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();

		openConfirm(0); // a1
		expect(confirmPanel()).not.toBeNull();

		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		// Still open, and nothing sent: the restore reload preserved it.
		expect(confirmPanel()).not.toBeNull();
		expect(deleteMock).not.toHaveBeenCalled();
	});

	it('keeps a row excluded while a SECOND delete of the same id is still pending (ref-count)', async () => {
		// A bare Set marker would let the FIRST delete's settle clear the id while a
		// SECOND delete of the same id is still outstanding, so a restore refetch
		// could repaint a row that is still being deleted (Codex round 2). The
		// in-flight-delete marker is ref-counted.
		props.parentArchived = true;
		props.canDelete = true;
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		mountStrip('item-a');
		await settle();
		expect(tiles()).toHaveLength(2);

		// First delete of a1 — deferred, and it will FAIL (so no tombstone latches;
		// only the in-flight marker stands between a1 and a repaint).
		let failFirst!: (e: Error) => void;
		deleteMock.mockReturnValueOnce(new Promise<void>((_, reject) => (failFirst = reject)));
		openConfirm(0); // a1
		clickConfirm();
		expect(tiles()).toHaveLength(1); // a1 optimistically removed

		// a1 reappears while its delete is still in flight (a re-announced upload —
		// it isn't tombstoned yet, so the row comes back), and the user deletes it
		// again: a SECOND outstanding delete of the same id.
		broadcastUpload('item-a', uploaded('a1'));
		flushSync();
		const del2 = deferred<void>();
		deleteMock.mockReturnValueOnce(del2.promise);
		openConfirm(0); // a1 again (newest-first, it is the first tile)
		clickConfirm();

		// The FIRST delete now fails and decrements ITS count — but the second is
		// still pending, so a1 must stay marked in-flight.
		failFirst(new Error('500'));
		await settle();

		// Restore: the refetch still lists a1. A bare Set (cleared by delete #1)
		// would repaint it; the ref-count keeps it excluded because delete #2 is
		// still outstanding.
		listMock.mockResolvedValueOnce(response([att({ id: 'a1' }), att({ id: 'a2' })]));
		props.parentArchived = false;
		flushSync();
		await settle();

		const names = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(names.some((n) => n.includes('a1'))).toBe(false);
		expect(names.some((n) => n.includes('a2'))).toBe(true);

		// Drain: the second delete now succeeds; the marker fully releases and a1
		// stays gone (now tombstoned). No leaked marker, no resurrection.
		del2.resolve();
		await settle();
		const final = tiles().map((el) => el.getAttribute('aria-label') ?? '');
		expect(final.some((n) => n.includes('a1'))).toBe(false);
		expect(final.some((n) => n.includes('a2'))).toBe(true);
	});

	// ── ORDINARY-LOAD in-flight-delete fence (PLAN-2392 3c-iii — load-path) ────
	//
	// U2 (TASK-2511) added the `inFlightDeletes` marker but applied it ONLY to the
	// restore revalidation's merge. The ORDINARY load paths — the mount/retry
	// response row filter, the pending-upload merge, and the load-FAILURE repaint —
	// filtered on `deletedIds` alone. `deletedIds` is latched only AFTER a delete's
	// API await (the deletion bus self-broadcast), so between an optimistic removal
	// and that broadcast a row is gone from `attachments` but not yet tombstoned. An
	// ordinary load whose response was issued (or is in flight) across that window
	// can still carry the row and repaint the just-removed tile — the same hole the
	// restore path already fences. These cover the three ordinary paths + the
	// rollback discipline that must still repaint a genuinely failed delete.

	it('does not repaint an in-flight-deleted row that an ordinary (retry) load returns', async () => {
		// Path 1 — the response ROW filter (~:453). a1 is painted from a server load
		// (surviving a failure as a pending upload → visible with a Retry), then a
		// retry load returns a1 while a1's delete is in flight. `pendingAtRequest`
		// already holds a1 at retry time, so the pending-upload merge excludes it via
		// `covered` — leaving the response row filter as the ONLY thing between a1 and
		// a repaint. That isolates path 1: dropping its `!isDeleting` guard resurrects
		// a1 even though the merge leg's guard is untouched.
		props.canDelete = true;
		let rejectLoad1!: (e: Error) => void;
		listMock.mockReturnValueOnce(
			new Promise<AttachmentListResponse>((_, reject) => (rejectLoad1 = reject))
		);
		mountStrip('item-a');
		flushSync();
		broadcastUpload('item-a', uploaded('a1'));
		flushSync();
		rejectLoad1(new Error('offline'));
		await settle();
		expect(tiles()).toHaveLength(1); // a1 kept as a pending upload
		expect(errorRow()).not.toBeNull();

		// Retry: a1 stays painted while the retry load is in flight (isRetry never blanks).
		const load2 = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(load2.promise);
		target.querySelector<HTMLElement>('.att-retry')!.click();
		flushSync();

		// Delete a1 — deferred, so its tombstone broadcast hasn't fired.
		const del = deferred<void>();
		deleteMock.mockReturnValueOnce(del.promise);
		openConfirm(0);
		clickConfirm();
		expect(tiles()).toHaveLength(0); // optimistic removal

		// The retry load lands mid-delete, still listing a1: the in-flight marker
		// filters it out of the response rows, so a1 does NOT resurrect.
		load2.resolve(response([att({ id: 'a1' })]));
		await settle();
		expect(tiles()).toHaveLength(0);

		// The delete resolves: still gone, now tombstoned too — the durable filter
		// takes over from the transient marker.
		del.resolve();
		await settle();
		expect(tiles()).toHaveLength(0);
		expect(deleteMock).toHaveBeenCalledWith('ws', 'a1');
	});

	it('does not resurrect an in-flight-deleted upload row through the pending-upload merge', async () => {
		// Path 2 — the pending-upload MERGE (~:468). a1 is a pending upload the mount
		// load's response does NOT return (the GET predates the upload), so a1 rides
		// back only via `missed`. The mount load's `pendingAtRequest` snapshot was
		// captured empty (before the upload), so `covered` is empty and cannot mask
		// the effect — this isolates path 2: dropping its `!isDeleting` guard
		// resurrects a1, while the response-row filter (raw is empty) is irrelevant.
		props.canDelete = true;
		const load1 = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(load1.promise);
		mountStrip('item-a');
		flushSync();
		broadcastUpload('item-a', uploaded('a1'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		const del = deferred<void>();
		deleteMock.mockReturnValueOnce(del.promise);
		openConfirm(0);
		clickConfirm();
		expect(tiles()).toHaveLength(0); // optimistic removal

		// The mount load resolves WITHOUT a1. a1 now lives only in the pending-upload
		// buffer; the merge must not ride it back in while its delete is in flight.
		load1.resolve(response([]));
		await settle();
		expect(tiles()).toHaveLength(0);

		del.resolve();
		await settle();
		expect(tiles()).toHaveLength(0);
	});

	it('does not resurrect an in-flight-deleted upload row through the load-failure repaint', async () => {
		// Path 3 — the load-FAILURE repaint (~:539). The catch arm repaints from the
		// pending-upload buffer, which still holds a1. The in-flight marker must keep
		// a1 out; dropping its `!isDeleting` guard resurrects a1 under the error row.
		props.canDelete = true;
		let rejectLoad1!: (e: Error) => void;
		listMock.mockReturnValueOnce(
			new Promise<AttachmentListResponse>((_, reject) => (rejectLoad1 = reject))
		);
		mountStrip('item-a');
		flushSync();
		broadcastUpload('item-a', uploaded('a1'));
		flushSync();
		expect(tiles()).toHaveLength(1);

		const del = deferred<void>();
		deleteMock.mockReturnValueOnce(del.promise);
		openConfirm(0);
		clickConfirm();
		expect(tiles()).toHaveLength(0); // optimistic removal

		// The mount load now FAILS: its catch repaints from the pending buffer, but
		// the in-flight marker keeps a1 out — no resurrection, just the error row.
		rejectLoad1(new Error('offline'));
		await settle();
		expect(errorRow()).not.toBeNull();
		expect(tiles()).toHaveLength(0);

		del.resolve();
		await settle();
		expect(tiles()).toHaveLength(0);
	});

	it('rolls a row back into view when its delete fails, even though a list response landed mid-delete', async () => {
		// The rollback discipline is a SETTLE-TIME correction and must survive the new
		// filter: a genuinely failed delete re-inserts the row DIRECTLY into
		// `attachments` (not through the filtered load paths), and the marker is
		// cleared in performDelete's `finally` — which runs AFTER the catch re-inserts
		// the row, so ordering never lets the filter swallow the rollback.
		props.canDelete = true;
		let rejectLoad1!: (e: Error) => void;
		listMock.mockReturnValueOnce(
			new Promise<AttachmentListResponse>((_, reject) => (rejectLoad1 = reject))
		);
		mountStrip('item-a');
		flushSync();
		broadcastUpload('item-a', uploaded('a1'));
		flushSync();
		rejectLoad1(new Error('offline'));
		await settle();
		expect(tiles()).toHaveLength(1);

		const load2 = deferred<AttachmentListResponse>();
		listMock.mockReturnValueOnce(load2.promise);
		target.querySelector<HTMLElement>('.att-retry')!.click();
		flushSync();

		// Delete a1 — the DELETE will fail.
		let rejectDel!: (e: Error) => void;
		deleteMock.mockReturnValueOnce(new Promise<void>((_, reject) => (rejectDel = reject)));
		openConfirm(0);
		clickConfirm();
		expect(tiles()).toHaveLength(0); // optimistic removal

		// The retry load lands mid-delete, still listing a1: the marker hides it, so
		// the tile does NOT come back on the load.
		load2.resolve(response([att({ id: 'a1' })]));
		await settle();
		expect(tiles()).toHaveLength(0);

		// The delete now FAILS: the row must roll back into view and toast.
		rejectDel(new Error('500'));
		await settle();
		expect(tiles()).toHaveLength(1);
		expect(tiles()[0].getAttribute('aria-label')).toContain('a1.png');
		expect(toastMock).toHaveBeenCalled();
	});
});
