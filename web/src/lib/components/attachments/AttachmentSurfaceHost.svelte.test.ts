import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';
import { _resetEscapeStackForTests } from '$lib/stores/escapeStack';
import type { LightboxImage } from '$lib/attachments/events';

// TASK-2487 / TASK-2490. The UNIFIED surface host is exercised through a DIRECT
// mount of the real `Lightbox` (the surface it hosts) so the acceptance can
// observe the whole chain: one event → one request → one mount → one probe → one
// focus return. TASK-2490 retired the cutover-window bridge (the host's two legacy
// subscriptions and their translation), so these tests drive the ONE surface
// channel end to end.
//
// jsdom cannot prove focus/inertness/layout the way TASK-2436's browser suite
// does; the assertions here are the manager-was-asked / structural-precondition
// shape the Lightbox suite established.
//
// MIGRATION MANIFEST (3c-ii T2b, TASK-2488). When the panel + the two legacy
// hosts were deleted, their suites consolidated as follows — named here so the
// coverage is accounted for, not silently dropped:
//   - AttachmentPanelHost.svelte.test.ts / AttachmentViewerHost.svelte.test.ts /
//     AttachmentViewerHostClose.svelte.test.ts — the ADDRESSING (DR-8 two-host
//     isolation), LIFECYCLE (archive/restore/item-switch/resourceGen/deletion),
//     and CLOSE (bound close handler) cases are the describes below, now against
//     the ONE host and the real Lightbox.
//   - The panel's own BEHAVIOR (metadata seed/fill, transient/missing states,
//     the delete drill-down, action rendering) is the Lightbox's now and is
//     covered by Lightbox.svelte.test.ts (190 tests).
//   - AttachmentDetailsPanel.extraction.test.ts — the machinery-extraction
//     grep-gate migrated to Lightbox.extraction.test.ts (same contract, new
//     consumer).
//   - The NodeView → host → Lightbox whole-route integration migrated in
//     editor/attachmentImageViewerHost.svelte.test.ts (mounts this host now).

// The Lightbox reads `viewport.isMobile` at load; pin desktop.
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return false;
		},
	},
	MOBILE_BREAKPOINT: 768,
	MOBILE_MEDIA_QUERY: '(max-width: 768px)',
}));

// The metadata machine's HEAD probe (the surface fires one when a seed field is
// null, or when an archived parent forces revalidation). Controlling it lets a
// test assert exactly-one-probe and drive the archived/restore reachability.
const metaFetch = vi.hoisted(() => vi.fn());
const metaRevalidate = vi.hoisted(() => vi.fn());
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (...a: unknown[]) => metaFetch(...a),
	revalidateAttachmentMetadata: (...a: unknown[]) => metaRevalidate(...a),
	invalidateAttachmentMetadata: vi.fn(),
}));

const { notifyAttachmentSurfaceOpen, notifyAttachmentDeleted } = await import(
	'$lib/attachments/events'
);
const { default: AttachmentSurfaceHost } = await import('./AttachmentSurfaceHost.svelte');

const ATT_A = '11111111-2222-4333-8444-555555555555';
const ATT_B = '99999999-8888-4777-8666-555555555555';

interface HostProps {
	itemId: string | null;
	hostToken: string;
	resourceGen: number;
	mutationsEnabled: boolean;
	getItemContent?: () => string | null;
	getLiveContent?: () => string | null;
	parentArchived: boolean;
}

function img(id: string, over: Partial<LightboxImage> = {}): LightboxImage {
	return {
		id,
		alt: 'an image',
		filename: 'pic.png',
		mime_type: 'image/png',
		size_bytes: 2048,
		width: 800,
		height: 600,
		...over,
	};
}

function surfaceEvent(over: Partial<Parameters<typeof notifyAttachmentSurfaceOpen>[0]> = {}) {
	return {
		attachmentId: ATT_A,
		workspaceSlug: 'ws-surface',
		itemId: 'item-a',
		hostToken: 'host-1',
		images: [img(ATT_A)],
		index: 0,
		invoker: null,
		filename: 'pic.png',
		mime_type: 'image/png',
		size_bytes: 2048,
		...over,
	};
}

/** Mounted surfaces — the Lightbox portals a `.lightbox-backdrop` to <body>. */
function surfaces(): HTMLElement[] {
	return Array.from(document.body.querySelectorAll<HTMLElement>('.lightbox-backdrop'));
}
function surfaceOpen(): boolean {
	return surfaces().length > 0;
}

async function settle() {
	for (let i = 0; i < 6; i++) await Promise.resolve();
	flushSync();
}

const mounted: ReturnType<typeof mount>[] = [];
let appRoot: HTMLElement;

// Reactive props, at module scope because `$state(...)` may only initialize a
// declaration. Two hosts: the pane runs a master + a peeked ItemDetail at once,
// which is exactly what DR-8 addressing exists for.
const propsA = $state<HostProps>({
	itemId: 'item-a',
	hostToken: 'host-1',
	resourceGen: 0,
	mutationsEnabled: true,
	parentArchived: false,
});
const propsB = $state<HostProps>({
	itemId: 'item-a',
	hostToken: 'host-2',
	resourceGen: 0,
	mutationsEnabled: true,
	parentArchived: false,
});

function mountHost(props: HostProps): ReturnType<typeof mount> {
	const app = mount(AttachmentSurfaceHost, { target: appRoot, props });
	mounted.push(app);
	flushSync();
	return app;
}

beforeEach(() => {
	metaFetch.mockReset();
	metaRevalidate.mockReset();
	metaFetch.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
	metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
	HTMLElement.prototype.getClientRects = function () {
		return [{}] as unknown as DOMRectList;
	};
	Object.assign(propsA, {
		itemId: 'item-a',
		hostToken: 'host-1',
		resourceGen: 0,
		mutationsEnabled: true,
		parentArchived: false,
	});
	Object.assign(propsB, {
		itemId: 'item-a',
		hostToken: 'host-2',
		resourceGen: 0,
		mutationsEnabled: true,
		parentArchived: false,
	});
	appRoot = document.body.appendChild(document.createElement('div'));
	appRoot.id = 'app';
});

afterEach(() => {
	while (mounted.length) unmount(mounted.pop()!);
	document.body.innerHTML = '';
	__resetViewerBackdropForTests();
	_resetEscapeStackForTests();
	vi.restoreAllMocks();
});

describe('AttachmentSurfaceHost — the surface channel (TASK-2487 / TASK-2490)', () => {
	it('opens the surface for an event on the SURFACE channel', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
	});

	it('EXACT-ONCE: an open fires exactly ONE forced no-store probe of the opened entry (T6)', async () => {
		// T6 always-revalidate-on-open: the OPENED entry is always revalidated with a
		// `no-store` HEAD — the host mints a per-open nonce, which forces the machine
		// through the revalidation path (not the plain seed-fill HEAD) so a cross-tab
		// delete the browser's max-age HEAD cache would hide is caught. Exactly ONE
		// probe, and it is the forced one.
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
		// One forced revalidation, none of the plain seed-fill fetches.
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaFetch).not.toHaveBeenCalled();
		// The forced probe carries `cache: 'no-store'` — inspect the OPTIONS the mock
		// actually received (4th arg), not a local constant.
		expect(metaRevalidate.mock.calls[0][3]).toEqual({ cache: 'no-store' });
	});

	it('REOPENING the same attachment fires a SECOND forced no-store probe (T6 nonce)', async () => {
		// The browser-cache-bypass proof shape: a complete-seed open would normally
		// short-circuit with zero HEADs, and even a re-probe could be served from the
		// HTTP cache. T6 mints a FRESH per-open nonce, which (a) remounts the surface
		// via {#key request} and (b) forces the machine down the no-store revalidation
		// path again — so reopening the SAME attachment issues a genuinely new HEAD,
		// not a cached answer. Two opens ⇒ two forced revalidations.
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);

		// Reopen the very same attachment (a new accepted open ⇒ a new nonce).
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
		expect(metaRevalidate).toHaveBeenCalledTimes(2);
		// Both forced probes carried no-store.
		expect(metaRevalidate.mock.calls[0][3]).toEqual({ cache: 'no-store' });
		expect(metaRevalidate.mock.calls[1][3]).toEqual({ cache: 'no-store' });
		// Never the plain seed-fill fetch for the opened entry.
		expect(metaFetch).not.toHaveBeenCalled();
	});

	it('arrowing to a fresh entry forces one no-store probe of the arrival (3c-iii U3), while arrowing back does not re-probe', async () => {
		// INVERTS the T6-era expectation. T6 forced only the OPENED entry; 3c-iii U3
		// forces one `no-store` probe per (open, entry) pair, so arrowing to a
		// not-yet-probed sibling revalidates it too (never the plain seed-fill fetch,
		// even though the sibling here has an INCOMPLETE seed). Arrowing BACK to the
		// already-probed first entry takes the fast path — no third probe.
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(
			surfaceEvent({ images: [img(ATT_A), img(ATT_B, { alt: 'second', size_bytes: null })] })
		);
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaFetch).not.toHaveBeenCalled();

		// Arrow to the sibling — a forced no-store revalidation of the arrival, NOT the
		// plain cacheable HEAD.
		document.body.querySelector<HTMLButtonElement>('.lightbox-nav.next')!.click();
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(2); // the arrival's forced probe
		expect(metaRevalidate.mock.calls[1][3]).toEqual({ cache: 'no-store' });
		expect(metaFetch).not.toHaveBeenCalled(); // never the plain seed-fill HEAD

		// Arrow BACK to the first entry — already probed under this nonce → no re-probe.
		document.body.querySelector<HTMLButtonElement>('.lightbox-nav.prev')!.click();
		await settle();
		expect(metaRevalidate).toHaveBeenCalledTimes(2);
	});

	it('keeps the CAPTURED workspace from the event, not any host default', async () => {
		// The surface channel captures its own workspace at emit; the host does not
		// substitute one. The download URL (built from the request wsSlug) proves it.
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(
			surfaceEvent({ workspaceSlug: 'ws-captured', mime_type: 'application/pdf', images: [img(ATT_A, { mime_type: 'application/pdf' })] })
		);
		await settle();
		const download = document.body.querySelector('.lightbox-toolbar [aria-label="Download"]');
		expect(download?.getAttribute('href')).toBe(`/api/v1/workspaces/ws-captured/attachments/${ATT_A}`);
	});

	it('invoker: focus returns to the event invoker on close, not the pre-open focus', async () => {
		// A sentinel holds focus at open, and the invoker is a DIFFERENT element that
		// is NOT focused beforehand. Lightbox falls back to the activeElement-at-open
		// when it gets no invoker, so returning focus to the invoker rather than the
		// sentinel is what proves the host forwarded `event.invoker`.
		const sentinel = document.body.appendChild(document.createElement('button'));
		sentinel.textContent = 'sentinel';
		const invoker = document.body.appendChild(document.createElement('button'));
		invoker.textContent = 'open';
		sentinel.focus();
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent({ invoker }));
		await settle();
		expect(surfaceOpen()).toBe(true);
		// Close the surface; focus returns to the invoker the event carried.
		document.body.querySelector<HTMLButtonElement>('.lightbox-close')!.click();
		await settle();
		expect(surfaceOpen()).toBe(false);
		expect(document.activeElement).toBe(invoker);
		expect(document.activeElement).not.toBe(sentinel);
	});

	it('null / disconnected invoker: closing does not throw and does not focus a gone node', async () => {
		const invoker = document.body.appendChild(document.createElement('button'));
		invoker.focus();
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent({ invoker }));
		await settle();
		// The invoker is detached while the surface is up.
		invoker.remove();
		document.body.querySelector<HTMLButtonElement>('.lightbox-close')!.click();
		await settle();
		expect(surfaceOpen()).toBe(false);
		expect(document.activeElement).not.toBe(invoker);
	});
});

describe('AttachmentSurfaceHost — addressing (DR-8, two live hosts)', () => {
	it('ignores an event addressed to the OTHER host, with both mounted', async () => {
		mountHost(propsA); // host-1
		mountHost(propsB); // host-2
		notifyAttachmentSurfaceOpen(surfaceEvent({ hostToken: 'host-2' }));
		await settle();
		// Exactly one surface — host-2's — not host-1's too.
		expect(surfaces()).toHaveLength(1);
	});

	it('ignores an event for a different item on its own token', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent({ itemId: 'item-z' }));
		await settle();
		expect(surfaceOpen()).toBe(false);
	});
});

describe('AttachmentSurfaceHost — parent lifecycle (DR-14)', () => {
	it('closes an open surface when the parent item is archived (transition)', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);

		propsA.parentArchived = true;
		await settle();
		expect(surfaceOpen()).toBe(false);
	});

	it('does NOT flash-close an ALREADY-archived open — it sits probe-gated inert', async () => {
		// The rule T3 deferred: opening a file whose parent is already archived is NOT
		// a transition, so the host does not close it; it mounts probe-gated (actions
		// inert until the reachability probe answers) rather than flash-closing a file
		// the user just asked for. Probe pinned pending so the inert state is stable.
		metaRevalidate.mockReturnValue(new Promise(() => {}));
		propsA.parentArchived = true;
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
		// Every toolbar control is inert while the archived probe is in flight.
		const tools = [...document.body.querySelectorAll('.lightbox-toolbar .lightbox-tool')];
		expect(tools.length).toBeGreaterThan(0);
		for (const t of tools) {
			const inert = t.hasAttribute('disabled') || t.getAttribute('aria-disabled') === 'true';
			expect(inert).toBe(true);
		}
	});

	it('revalidates an archived-at-open surface when the parent is restored', async () => {
		// Archived-at-open, probe hangs → inert. Restore bumps revalidateToken → the
		// surface re-probes and, now reachable, its actions come live.
		let releaseProbe: ((v: unknown) => void) | undefined;
		metaRevalidate.mockReturnValueOnce(new Promise((resolve) => (releaseProbe = resolve)));
		propsA.parentArchived = true;
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		const download = () => document.body.querySelector('.lightbox-toolbar [aria-label="Download"]');
		expect(download()?.getAttribute('aria-disabled')).toBe('true');

		// Restore: the re-probe resolves reachable.
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 1536 });
		releaseProbe?.({ status: 'ok', mime: 'application/pdf', size: 1536 });
		propsA.parentArchived = false;
		await settle();
		expect(surfaceOpen()).toBe(true);
		expect(download()?.getAttribute('aria-disabled')).not.toBe('true');
	});
});

describe('AttachmentSurfaceHost — resource switch + deletion', () => {
	it('closes when the host switches item', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);

		propsA.itemId = 'item-b';
		await settle();
		expect(surfaceOpen()).toBe(false);
	});

	it('closes when the resource generation advances on the same item', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);

		propsA.resourceGen = 1;
		await settle();
		expect(surfaceOpen()).toBe(false);
	});

	it('re-targets in place when a second attachment opens, dropping the first', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaces()).toHaveLength(1);

		notifyAttachmentSurfaceOpen(surfaceEvent({ attachmentId: ATT_B, images: [img(ATT_B)] }));
		await settle();
		// Still exactly one surface — the {#key request} remount replaced it.
		expect(surfaces()).toHaveLength(1);
		const download = document.body.querySelector('.lightbox-toolbar [aria-label="Download"]');
		expect(download?.getAttribute('href')).toContain(ATT_B);
	});

	it('a single open whose file 404s shows the inert overlay, not a close', async () => {
		// A single-attachment open whose file turns out to be gone must show
		// "no longer available" rather than flash-closing. T6: the opened entry is
		// always force-revalidated, so the 404 arrives on the revalidation probe.
		metaRevalidate.mockResolvedValue({ status: 'missing' });
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		// Still open — the surface did not flash-close.
		expect(surfaceOpen()).toBe(true);
		const missing = document.body.querySelector('.lightbox-missing');
		expect(missing).not.toBeNull();
		expect(missing?.textContent).toContain('no longer available');
	});

	it('closes a SINGLE-open surface when another surface deletes its attachment', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);

		notifyAttachmentDeleted(ATT_A);
		await settle();
		expect(surfaceOpen()).toBe(false);
	});

	it('does NOT close a multi-image SET on an external delete — the tombstone path advances', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(
			surfaceEvent({ images: [img(ATT_A), img(ATT_B, { alt: 'second' })] })
		);
		await settle();
		expect(surfaceOpen()).toBe(true);

		// Deleting one member of a set: the Lightbox advances to the survivor; the
		// host must not preempt with a close.
		notifyAttachmentDeleted(ATT_A);
		await settle();
		expect(surfaceOpen()).toBe(true);
	});
});
