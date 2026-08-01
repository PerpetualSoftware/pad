import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { AttachmentListItem, AttachmentListResponse } from '$lib/types';

// TASK-2383. The strip is mounted OUTSIDE ItemDetail's `{#key itemSlug}`
// block, so it PERSISTS across an A→B item switch — the no-{#key} bug class
// from PLAN-2105 / TASK-2112. These tests mount the real component and drive
// its fetch to prove the generation fence holds when A's response resolves
// after B's request went out.

const listMock =
	vi.fn<(ws: string, filters: Record<string, unknown>) => Promise<AttachmentListResponse>>();

vi.mock('$lib/api/client', () => ({
	api: {
		attachments: {
			list: (ws: string, filters: Record<string, unknown>) => listMock(ws, filters),
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
		},
	},
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
const props = $state<{ wsSlug: string; username: string; itemId: string | null }>({
	wsSlug: 'ws',
	username: 'dave',
	itemId: null,
});

describe('ItemAttachmentStrip', () => {
	let target: HTMLElement;
	let instance: ReturnType<typeof mount> | undefined;

	beforeEach(() => {
		listMock.mockReset();
		props.wsSlug = 'ws';
		props.username = 'dave';
		props.itemId = null;
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

	it('labels tiles with filename + human size and links non-images to a download', async () => {
		listMock.mockResolvedValue(
			response([
				att({ id: 'doc', mime_type: 'application/pdf', filename: 'spec.pdf', size_bytes: 1536 }),
			])
		);
		mountStrip('item-a');
		await settle();

		const tile = tiles()[0];
		expect(tile.tagName).toBe('A');
		expect(tile.getAttribute('aria-label')).toBe('spec.pdf (1.5 KB)');
		expect(tile.getAttribute('title')).toBe('spec.pdf (1.5 KB)');
		expect(tile.getAttribute('href')).toBe('/api/v1/workspaces/ws/attachments/doc');
		expect(tile.getAttribute('download')).toBe('spec.pdf');
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
		const more = target.querySelector<HTMLElement>('.att-more');
		expect(more?.textContent?.trim()).toBe('+4');

		more?.click();
		flushSync();
		expect(tiles()).toHaveLength(12);
		expect(target.querySelector('.att-more')).toBeNull();
	});

	it('links out to Settings → Storage once expanded at the 50-row bound', async () => {
		const rows = Array.from({ length: 50 }, (_, i) => att({ id: `a${i}` }));
		listMock.mockResolvedValue({ attachments: rows, total: 120, limit: 50, offset: 0 });
		mountStrip('item-a');
		await settle();

		expect(target.querySelector<HTMLElement>('.att-more')?.textContent?.trim()).toBe('+42');
		target.querySelector<HTMLElement>('.att-more')?.click();
		flushSync();

		const link = target.querySelector<HTMLAnchorElement>('a.att-more');
		expect(link?.getAttribute('href')).toBe('/dave/ws/settings#storage');
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

	it('renders nothing when the fetch fails', async () => {
		listMock.mockRejectedValue(new Error('boom'));
		mountStrip('item-a');
		await settle();

		expect(target.querySelector('.attachment-strip')).toBeNull();
	});
});
