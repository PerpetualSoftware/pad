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
vi.mock('$lib/attachments/deletion', () => ({
	notifyAttachmentDeleted: (uuid: string) => notifyDeletedMock(uuid),
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
}));

const invalidateMock = vi.fn<(ws: string, uuid: string) => void>();
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	invalidateAttachmentMetadata: (ws: string, uuid: string) => invalidateMock(ws, uuid),
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
}>({
	wsSlug: 'ws',
	username: 'dave',
	itemId: null,
	canDelete: false,
	itemContent: null,
	liveContent: null,
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
