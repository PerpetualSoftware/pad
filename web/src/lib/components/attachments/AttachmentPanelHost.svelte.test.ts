import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { AttachmentMetadataResult } from '$lib/components/editor/attachment-metadata';

// TASK-2423. The options panel is exercised THROUGH its host, because the host
// is where the two rules that matter live: an event is consumed only when both
// `itemId` and `hostToken` are this host's own (DR-8), and the permission the
// panel's Delete uses comes from the host rather than from the emitting
// surface. Nothing routes INTO the panel yet — the strip's tiles and the
// editor's chips start emitting in the next task — so these tests emit on the
// bus directly, which is also the only way to drive a NodeView-originated open.
//
// What jsdom CANNOT prove here, and is therefore phase 3d's browser suite:
// focus entry/return, background inertness, the desktop popover's real
// placement, the mobile sheet swap, and Enter/Space activation of the rows.

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
			downloadUrl: (ws: string, id: string, variant?: string) =>
				`/api/v1/workspaces/${ws}/attachments/${id}${variant ? `?variant=${variant}` : ''}`,
			delete: (ws: string, id: string) => deleteMock(ws, id),
		},
	},
}));

// The metadata cache is mocked so a test can hand back each arm of the typed
// result (DR-10) and, crucially, assert WHICH entry point was used: Retry must
// go through `revalidate*` (invalidate-then-fetch), because a plain refetch
// replays the cached failure and looks broken.
const fetchMetaMock = vi.fn<() => Promise<AttachmentMetadataResult>>();
const revalidateMetaMock = vi.fn<() => Promise<AttachmentMetadataResult>>();
const invalidateMetaMock = vi.fn<(ws: string, id: string) => void>();
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: () => fetchMetaMock(),
	revalidateAttachmentMetadata: () => revalidateMetaMock(),
	invalidateAttachmentMetadata: (ws: string, id: string) => invalidateMetaMock(ws, id),
}));

vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (message: string, kind?: string) => toastMock(message, kind) },
}));

// The events bus stays REAL — addressing is the thing under test — with only
// the deletion broadcast wrapped so a test can assert the panel announces
// exactly as the strip's tile does.
const announceMock = vi.fn<(ws: string, id: string) => void>();
vi.mock('$lib/attachments/events', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/attachments/events')>();
	return {
		...actual,
		announceAttachmentDeleted: (ws: string, id: string) => announceMock(ws, id),
	};
});

const { notifyAttachmentPanelOpen } = await import('$lib/attachments/events');
const { default: AttachmentPanelHost } = await import('./AttachmentPanelHost.svelte');

interface HostProps {
	wsSlug: string;
	itemId: string | null;
	hostToken: string;
	mutationsEnabled: boolean;
	itemContent: string | null;
	liveContent: (() => string | null) | null;
	parentArchived: boolean;
}

// A canonical UUID: `attachmentRefsIn` only recognizes the 36-char form, so
// the "still used in this item's content" warning is only exercised with a
// real id.
const ATT_ID = '11111111-2222-4333-8444-555555555555';
const ATT_ID_2 = '99999999-8888-4777-8666-555555555555';

function openEvent(overrides: Partial<Parameters<typeof notifyAttachmentPanelOpen>[0]> = {}) {
	return {
		attachmentId: ATT_ID,
		itemId: 'item-a',
		hostToken: 'host-1',
		anchor: null,
		filename: 'spec.pdf',
		mime_type: 'application/pdf',
		size_bytes: 1536,
		...overrides,
	};
}

/** Rows are portaled to <body>, so queries are document-wide by necessity. */
function panel(): HTMLElement | null {
	return document.querySelector<HTMLElement>('[role="menu"]');
}

function rows(): HTMLElement[] {
	return Array.from(document.querySelectorAll<HTMLElement>('[role="menu"] [role="menuitem"]'));
}

/** By VISIBLE label — MenuItem's icon span is part of `textContent`. */
function row(label: string): HTMLElement | undefined {
	return rows().find((el) => el.querySelector('.mi-label')?.textContent?.trim() === label);
}

async function settle() {
	await Promise.resolve();
	await Promise.resolve();
	await Promise.resolve();
	flushSync();
}

// Reactive props objects, declared at the top level because `$state(...)` may
// only initialize a declaration. Two of them: the pane host runs a master and a
// peeked ItemDetail at once, and that concurrency is exactly what DR-8's
// addressing exists for.
const propsA = $state<HostProps>({
	wsSlug: 'ws',
	itemId: 'item-a',
	hostToken: 'host-1',
	mutationsEnabled: true,
	itemContent: null,
	liveContent: null,
	parentArchived: false,
});
const propsB = $state<HostProps>({
	wsSlug: 'ws',
	itemId: 'item-a',
	hostToken: 'host-2',
	mutationsEnabled: false,
	itemContent: null,
	liveContent: null,
	parentArchived: false,
});

describe('AttachmentPanelHost', () => {
	let target: HTMLElement;
	const mounted: ReturnType<typeof mount>[] = [];

	beforeEach(() => {
		deleteMock.mockReset();
		deleteMock.mockResolvedValue(undefined);
		toastMock.mockReset();
		announceMock.mockReset();
		fetchMetaMock.mockReset();
		fetchMetaMock.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 2048 });
		revalidateMetaMock.mockReset();
		revalidateMetaMock.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 2048 });
		invalidateMetaMock.mockReset();
		Object.assign(propsA, {
			wsSlug: 'ws',
			itemId: 'item-a',
			hostToken: 'host-1',
			mutationsEnabled: true,
			itemContent: null,
			liveContent: null,
			parentArchived: false,
		});
		Object.assign(propsB, {
			wsSlug: 'ws',
			itemId: 'item-a',
			hostToken: 'host-2',
			mutationsEnabled: false,
			itemContent: null,
			liveContent: null,
			parentArchived: false,
		});
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		while (mounted.length) unmount(mounted.pop()!);
		target.remove();
	});

	function mountHost(props: HostProps) {
		mounted.push(mount(AttachmentPanelHost, { target, props }));
		flushSync();
	}

	it('opens for an event addressed to it', () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		flushSync();

		expect(panel()).not.toBeNull();
		expect(panel()?.textContent).toContain('spec.pdf');
	});

	it('ignores an event addressed to the OTHER host, with both mounted', () => {
		mountHost(propsA);
		mountHost(propsB);

		// Same item, other host token: only one panel may open, and it must be
		// the addressed one. Matching on itemId alone would open two.
		notifyAttachmentPanelOpen(openEvent({ hostToken: 'host-2' }));
		flushSync();

		const panels = document.querySelectorAll('[role="menu"]');
		expect(panels).toHaveLength(1);
		// host-2 is the peeked pane in this fixture (mutationsEnabled false), so
		// the panel that opened must be the one WITHOUT a live Delete.
		expect((row('Delete') as HTMLButtonElement | undefined)?.disabled).toBe(true);
	});

	it('ignores an event for a different item on its own token', () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent({ itemId: 'item-b' }));
		flushSync();

		expect(panel()).toBeNull();
	});

	it('renders the actions from the shared descriptor list, honouring element', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		// PDF previews natively, so Open applies; both anchors are real
		// <a download>/<a target> elements, not buttons that navigate.
		const open = row('Open in new tab');
		expect(open?.tagName).toBe('A');
		expect(open?.getAttribute('target')).toBe('_blank');
		expect(open?.getAttribute('rel')).toBe('noopener noreferrer');
		const download = row('Download');
		expect(download?.tagName).toBe('A');
		expect(download?.getAttribute('href')).toBe(`/api/v1/workspaces/ws/attachments/${ATT_ID}`);
		expect(download?.getAttribute('download')).toBe('spec.pdf');
		expect(row('Copy workspace link')?.tagName).toBe('BUTTON');
		expect(row('Delete')?.tagName).toBe('BUTTON');
	});

	it('omits Open for a type the browser cannot preview', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(
			openEvent({ mime_type: 'application/zip', filename: 'logs.zip' })
		);
		await settle();

		// Absent, never disabled: a greyed Open implies a preview Pad could
		// give and won't.
		expect(row('Open in new tab')).toBeUndefined();
		expect(row('Download')).toBeDefined();
		expect(panel()?.textContent).toContain('ZIP archive');
	});

	it('opens IMMEDIATELY with partial metadata, then completes it', async () => {
		let resolveMeta!: (r: AttachmentMetadataResult) => void;
		fetchMetaMock.mockReturnValue(
			new Promise<AttachmentMetadataResult>((r) => (resolveMeta = r))
		);
		mountHost(propsA);
		// A chip's HEAD probe may not have completed: all three fields null.
		notifyAttachmentPanelOpen(
			openEvent({ filename: null, mime_type: null, size_bytes: null })
		);
		flushSync();

		// Painted before the fetch settles — never a blank sheet, never a wait.
		expect(panel()).not.toBeNull();
		expect(panel()?.textContent).toContain('Attachment');
		expect(panel()?.textContent).toContain('Reading details…');

		resolveMeta({ status: 'ok', mime: 'application/pdf', size: 1024 });
		await settle();
		expect(panel()?.textContent).toContain('PDF');
		expect(panel()?.textContent).toContain('1.0 KB');
	});

	it('does not fetch when the event carried all three fields', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		// The strip's entry point always has them from its list row.
		expect(fetchMetaMock).not.toHaveBeenCalled();
		expect(panel()?.textContent).toContain('1.5 KB');
	});

	it('shows an inline retryable error on a transient failure, keeping what it knows', async () => {
		fetchMetaMock.mockResolvedValue({ status: 'transient' });
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent({ mime_type: null, size_bytes: null }));
		await settle();

		expect(panel()?.textContent).toContain("Couldn't load the file details.");
		// Beside the row it already knows, not instead of it.
		expect(panel()?.textContent).toContain('spec.pdf');
		// Actions stay live: transient says NOTHING about whether the row exists.
		expect((row('Download') as HTMLElement).tagName).toBe('A');

		revalidateMetaMock.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 4096 });
		row('Retry')!.click();
		await settle();

		// Retry INVALIDATES before refetching — a plain refetch would replay the
		// cached failure (DR-10).
		expect(revalidateMetaMock).toHaveBeenCalledTimes(1);
		expect(panel()?.textContent).not.toContain("Couldn't load the file details.");
		expect(panel()?.textContent).toContain('4.0 KB');
	});

	it('latches an authoritative missing state and makes every action inert', async () => {
		fetchMetaMock.mockResolvedValue({ status: 'missing' });
		mountHost(propsA);
		// Size unknown, so the panel fetches; the MIME is known, so Open is in
		// the rendered set and its inertness is observable too.
		notifyAttachmentPanelOpen(openEvent({ size_bytes: null }));
		await settle();

		expect(panel()?.textContent).toContain('This file is no longer available.');
		expect(panel()?.textContent).toContain('No longer available');
		// A disabled anchor is not a thing, so MenuItem falls back to a disabled
		// button — the row is inert AND skipped by the menu's keyboard walk.
		for (const label of ['Open in new tab', 'Download', 'Copy workspace link', 'Delete']) {
			const el = row(label) as HTMLButtonElement | undefined;
			expect(el?.tagName).toBe('BUTTON');
			expect(el?.disabled).toBe(true);
		}
	});

	it('takes Delete permission from the HOST: peeked pane cannot, master can', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();
		expect((row('Delete') as HTMLButtonElement).disabled).toBe(false);

		// Peek freezes this side; the event said nothing about permission and
		// must not be able to.
		propsA.mutationsEnabled = false;
		flushSync();
		expect((row('Delete') as HTMLButtonElement).disabled).toBe(true);
	});

	it('deletes through an in-app drill-down confirmation, Cancel first', async () => {
		propsA.itemContent = `body with ![x](pad-attachment:${ATT_ID}) inline`;
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		row('Delete')!.click();
		await settle();

		// The confirmation is a sub-view of the panel, not a window.confirm.
		const prompt = document.querySelector('.attachment-delete-prompt');
		expect(prompt?.getAttribute('role')).toBe('presentation');
		expect(prompt?.textContent).toContain("still used in this item's content");
		const confirmRows = rows();
		const labelOf = (el: HTMLElement) => el.querySelector('.mi-label')?.textContent?.trim();
		expect(labelOf(confirmRows[0])).toBe('Cancel');
		expect(labelOf(confirmRows[confirmRows.length - 1])).toBe('Delete file');
		// The destructive row points back at the prompt, which is otherwise
		// never announced.
		expect(confirmRows[confirmRows.length - 1].getAttribute('aria-describedby')).toBe(
			prompt?.id
		);
		expect(deleteMock).not.toHaveBeenCalled();

		row('Delete file')!.click();
		await settle();

		expect(deleteMock).toHaveBeenCalledWith('ws', ATT_ID);
		// Exactly what the tile does, so the strip and the editor reconcile.
		expect(announceMock).toHaveBeenCalledWith('ws', ATT_ID);
		expect(panel()).toBeNull();
	});

	it('warns honestly when the attachment is not referenced in this body', async () => {
		propsA.itemContent = 'nothing here';
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		row('Delete')!.click();
		await settle();
		expect(document.querySelector('.attachment-delete-prompt')?.textContent).toContain(
			"isn't referenced in this item's content"
		);
	});

	it('reads the LIVE editor markdown for the in-use warning, not just the saved body', async () => {
		// The persisted body lags the editor, so an image inserted seconds ago
		// would otherwise slip past the warning.
		propsA.itemContent = 'nothing here';
		propsA.liveContent = () => `just pasted ![x](pad-attachment:${ATT_ID})`;
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		row('Delete')!.click();
		await settle();
		expect(document.querySelector('.attachment-delete-prompt')?.textContent).toContain(
			"still used in this item's content"
		);
	});

	it('cancelling the confirmation sends no request and returns to the actions', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		row('Delete')!.click();
		await settle();
		row('Cancel')!.click();
		await settle();

		expect(deleteMock).not.toHaveBeenCalled();
		expect(row('Download')).toBeDefined();
		expect(panel()).not.toBeNull();
	});

	it('surfaces a failed delete inline and leaves the panel open', async () => {
		deleteMock.mockRejectedValue(new Error('Network unreachable'));
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();

		row('Delete')!.click();
		await settle();
		row('Delete file')!.click();
		await settle();

		expect(panel()?.textContent).toContain('Network unreachable');
		expect(announceMock).not.toHaveBeenCalled();
	});

	it('closes an open panel when the parent item is archived (DR-14)', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();
		expect(panel()).not.toBeNull();

		// An archived parent's attachment fetch returns a generic 404, so an
		// open panel would keep offering an Open and a Download that both fail.
		// The strip sits outside ItemDetail's keyed lifecycle block, so this is
		// added, not inherited.
		propsA.parentArchived = true;
		await settle();
		expect(panel()).toBeNull();
	});

	it('revalidates an open panel when the parent item is restored (DR-14)', async () => {
		// Opened while the parent was already archived: the fetch 404s, so the
		// panel latches the authoritative missing state.
		propsA.parentArchived = true;
		fetchMetaMock.mockResolvedValue({ status: 'missing' });
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent({ mime_type: null, size_bytes: null }));
		await settle();
		expect(panel()?.textContent).toContain('This file is no longer available.');

		// Restore does NOT assume the previous state still holds — it re-reads
		// through the invalidating path, and the panel comes back to life.
		revalidateMetaMock.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 1536 });
		propsA.parentArchived = false;
		await settle();
		expect(revalidateMetaMock).toHaveBeenCalledTimes(1);
		expect(panel()?.textContent).not.toContain('This file is no longer available.');
		expect((row('Download') as HTMLElement).tagName).toBe('A');
	});

	it('closes when the host switches item', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();
		expect(panel()).not.toBeNull();

		propsA.itemId = 'item-b';
		await settle();
		expect(panel()).toBeNull();
	});

	it('re-targets in place when a second attachment is opened, dropping the first state', async () => {
		fetchMetaMock.mockResolvedValue({ status: 'missing' });
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent({ mime_type: null, size_bytes: null }));
		await settle();
		expect(panel()?.textContent).toContain('This file is no longer available.');

		// The panel is NOT re-keyed per attachment, so the previous subject's
		// latched state has to be cleared explicitly.
		notifyAttachmentPanelOpen(
			openEvent({ attachmentId: ATT_ID_2, filename: 'notes.txt', mime_type: 'text/plain', size_bytes: 12 })
		);
		await settle();
		expect(panel()?.textContent).not.toContain('This file is no longer available.');
		expect(panel()?.textContent).toContain('notes.txt');
		expect((row('Download') as HTMLElement).getAttribute('href')).toBe(
			`/api/v1/workspaces/ws/attachments/${ATT_ID_2}`
		);
	});

	// --- stale-continuation regressions (orchestrator review) -----------------
	// All three are the same shape: something the panel started keeps running
	// after the host has moved on, and lands on whatever is on screen by then.

	it("a delete resolving after an item switch does not close the panel opened since", async () => {
		mountHost(propsA);
		let releaseDelete: (() => void) | undefined;
		deleteMock.mockImplementation(
			() => new Promise<void>((resolve) => (releaseDelete = () => resolve()))
		);

		// Attachment A: confirm the delete, leaving the request in flight.
		notifyAttachmentPanelOpen(openEvent());
		await settle();
		(row('Delete') as HTMLElement).click();
		await settle();
		(row('Delete file') as HTMLElement).click();
		await settle();

		// The host switches items, destroying that panel...
		propsA.itemId = 'item-b';
		flushSync();
		expect(panel()).toBeNull();

		// ...and a panel opens on a DIFFERENT attachment for the new item.
		notifyAttachmentPanelOpen(
			openEvent({ itemId: 'item-b', attachmentId: ATT_ID_2, filename: 'notes.txt' })
		);
		await settle();
		expect(panel()?.textContent).toContain('notes.txt');

		// Now the first delete finally lands.
		releaseDelete?.();
		await settle();

		// It must not dismiss the panel the user just opened.
		expect(panel()).not.toBeNull();
		expect(panel()?.textContent).toContain('notes.txt');
	});

	it('resolves a confirmation left on screen when the host tears the panel down', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent());
		await settle();
		(row('Delete') as HTMLElement).click();
		await settle();
		expect(panel()?.textContent).toContain('Delete file');

		// Archive destroys the child mid-confirmation. The descriptor is
		// awaiting that promise; leaving it unresolved strands the await (and
		// the closure it holds) forever.
		propsA.parentArchived = true;
		await settle();
		expect(panel()).toBeNull();

		// Nothing was deleted, and no late continuation runs.
		expect(deleteMock).not.toHaveBeenCalled();
		expect(announceMock).not.toHaveBeenCalled();
	});

	it('shows a retryable error when the restore revalidation fails, instead of a dead end', async () => {
		// The reachable shape of DR-14's restore path: archiving CLOSES an open
		// panel, so a forced revalidation only ever lands on a panel opened
		// while the parent was ALREADY archived — where the attachment fetch
		// legitimately 404s.
		propsA.parentArchived = true;
		mountHost(propsA);
		fetchMetaMock.mockResolvedValue({ status: 'missing' });
		// Metadata gaps, so the panel actually probes — a chip's event, not the
		// strip's (which carries all three and skips the fetch).
		const partial = { mime_type: null, size_bytes: null };
		notifyAttachmentPanelOpen(openEvent(partial));
		await settle();
		expect(panel()?.textContent).toContain('This file is no longer available.');
		// Authoritative-missing offers no Retry, by design.
		expect(row('Retry')).toBeUndefined();

		// The item is restored, so that 404 no longer necessarily holds — but
		// the revalidation itself fails transiently. Leaving `missing` latched
		// would show "no longer available" with no way to ask again: the exact
		// empty-vs-broken dead end DR-10 exists to prevent.
		revalidateMetaMock.mockResolvedValue({ status: 'transient' });
		propsA.parentArchived = false;
		await settle();

		expect(revalidateMetaMock).toHaveBeenCalled();
		expect(panel()?.textContent).not.toContain('This file is no longer available.');
		expect(row('Retry')).toBeDefined();
	});

	it('keeps the full filename in the accessible name while the visible row truncates', async () => {
		const long = `${'a'.repeat(200)}.pdf`;
		mountHost(propsA);
		notifyAttachmentPanelOpen(openEvent({ filename: long }));
		await settle();

		// Truncation is a visual affordance, never an information loss (DR-13).
		expect(panel()?.getAttribute('aria-label')).toContain(long);
		expect(document.querySelector('.ap-name')?.getAttribute('title')).toBe(long);
	});
});
