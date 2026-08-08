import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';
import { _resetEscapeStackForTests } from '$lib/stores/escapeStack';
import type { LightboxImage } from '$lib/attachments/events';

// TASK-2487. The UNIFIED surface host is exercised through a DIRECT mount of the
// real `Lightbox` (the surface it hosts) so the acceptance can observe the whole
// chain: one event → one request → one mount → one probe → one focus return. The
// host is mounted NOWHERE in the app yet (T2b cuts it in); these tests + a grep
// are its only consumers.
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

// The bus stays REAL — addressing + the bridge are the things under test — with
// only the surface NOTIFIER spied, so a test can pin the bridge invariant: the
// host translates legacy events INTERNALLY and never re-broadcasts on the public
// surface channel.
const surfaceNotifySpy = vi.hoisted(() => vi.fn());
vi.mock('$lib/attachments/events', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/attachments/events')>();
	return {
		...actual,
		notifyAttachmentSurfaceOpen: (event: unknown) => {
			surfaceNotifySpy(event);
			return actual.notifyAttachmentSurfaceOpen(event as never);
		},
	};
});

const {
	notifyAttachmentPanelOpen,
	notifyViewerOpen,
	notifyAttachmentSurfaceOpen,
	notifyAttachmentDeleted,
} = await import('$lib/attachments/events');
const { default: AttachmentSurfaceHost } = await import('./AttachmentSurfaceHost.svelte');

const ATT_A = '11111111-2222-4333-8444-555555555555';
const ATT_B = '99999999-8888-4777-8666-555555555555';

interface HostProps {
	wsSlug: string;
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

function panelEvent(over: Partial<Parameters<typeof notifyAttachmentPanelOpen>[0]> = {}) {
	return {
		attachmentId: ATT_A,
		itemId: 'item-a',
		hostToken: 'host-1',
		anchor: null,
		filename: 'spec.pdf',
		mime_type: 'application/pdf',
		size_bytes: 1536,
		...over,
	};
}

function viewerEvent(over: Partial<Parameters<typeof notifyViewerOpen>[0]> = {}) {
	return {
		attachmentId: ATT_A,
		workspaceSlug: 'ws-viewer',
		itemId: 'item-a',
		hostToken: 'host-1',
		images: [img(ATT_A)],
		index: 0,
		invoker: null,
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
	wsSlug: 'ws',
	itemId: 'item-a',
	hostToken: 'host-1',
	resourceGen: 0,
	mutationsEnabled: true,
	parentArchived: false,
});
const propsB = $state<HostProps>({
	wsSlug: 'ws',
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
	surfaceNotifySpy.mockReset();
	metaFetch.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
	metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
	HTMLElement.prototype.getClientRects = function () {
		return [{}] as unknown as DOMRectList;
	};
	Object.assign(propsA, {
		wsSlug: 'ws',
		itemId: 'item-a',
		hostToken: 'host-1',
		resourceGen: 0,
		mutationsEnabled: true,
		parentArchived: false,
	});
	Object.assign(propsB, {
		wsSlug: 'ws',
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

describe('AttachmentSurfaceHost — the three-channel bridge (TASK-2487)', () => {
	it('opens the surface for an event on the SURFACE channel', async () => {
		mountHost(propsA);
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
	});

	it('opens the surface for a legacy PANEL event, translated internally', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
		// The bridge invariant: the host never re-broadcasts on the public notifier.
		expect(surfaceNotifySpy).not.toHaveBeenCalled();
	});

	it('opens the surface for a legacy VIEWER event, translated internally', async () => {
		mountHost(propsA);
		notifyViewerOpen(viewerEvent());
		await settle();
		expect(surfaceOpen()).toBe(true);
		expect(surfaceNotifySpy).not.toHaveBeenCalled();
	});

	it('EXACT-ONCE: a legacy + surface double-emission for the same open does not double-open', async () => {
		mountHost(propsA);
		// A producer mid-transition emits BOTH the legacy viewer event and the new
		// surface event for one open. One request state → at most one mount.
		notifyViewerOpen(viewerEvent());
		notifyAttachmentSurfaceOpen(surfaceEvent());
		await settle();
		expect(surfaces()).toHaveLength(1);
	});

	it('EXACT-ONCE: a legacy panel event fires exactly ONE metadata probe', async () => {
		// Incomplete seed (size null) → the surface completes it with a single HEAD.
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent({ size_bytes: null }));
		await settle();
		expect(surfaceOpen()).toBe(true);
		expect(metaFetch).toHaveBeenCalledTimes(1);
	});
});

describe('AttachmentSurfaceHost — translation fidelity (TASK-2487)', () => {
	it('PANEL → single-open surface with the host wsSlug (the transitional exception)', async () => {
		// The panel channel carries no workspace; the bridge supplies the host's, and
		// the download URL (built from the surface wsSlug) proves it.
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent({ mime_type: 'application/pdf' }));
		await settle();
		const download = document.body.querySelector('.lightbox-toolbar [aria-label="Download"]');
		expect(download?.getAttribute('href')).toBe(`/api/v1/workspaces/ws/attachments/${ATT_A}`);
	});

	it('a STALE panel event uses the host wsSlug at emit, not the event', async () => {
		// The pane switches workspace without remounting; a legacy panel event has no
		// captured workspace, so the bridge reads the host's CURRENT prop.
		propsA.wsSlug = 'ws-current';
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent({ mime_type: 'application/pdf' }));
		await settle();
		const download = document.body.querySelector('.lightbox-toolbar [aria-label="Download"]');
		expect(download?.getAttribute('href')).toBe(
			`/api/v1/workspaces/ws-current/attachments/${ATT_A}`
		);
	});

	it('VIEWER → surface field-for-field, keeping the CAPTURED workspace (not the host)', async () => {
		// The viewer channel captured its own workspace at emit; the bridge must keep
		// it, not substitute the host's.
		propsA.wsSlug = 'ws-host';
		mountHost(propsA);
		notifyViewerOpen(viewerEvent({ workspaceSlug: 'ws-captured' }));
		await settle();
		const download = document.body.querySelector('.lightbox-toolbar [aria-label="Download"]');
		expect(download?.getAttribute('href')).toBe(`/api/v1/workspaces/ws-captured/attachments/${ATT_A}`);
	});

	it('PANEL anchor → invoker: focus returns to the anchor on close', async () => {
		const anchor = document.body.appendChild(document.createElement('button'));
		anchor.textContent = 'open';
		anchor.focus();
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent({ anchor }));
		await settle();
		expect(surfaceOpen()).toBe(true);
		// Close the surface; focus returns to the anchor the panel event carried.
		document.body.querySelector<HTMLButtonElement>('.lightbox-close')!.click();
		await settle();
		expect(surfaceOpen()).toBe(false);
		expect(document.activeElement).toBe(anchor);
	});

	it('PANEL null / disconnected anchor: closing does not throw and does not focus a gone node', async () => {
		const anchor = document.body.appendChild(document.createElement('button'));
		anchor.focus();
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent({ anchor }));
		await settle();
		// The anchor is detached while the surface is up.
		anchor.remove();
		document.body.querySelector<HTMLButtonElement>('.lightbox-close')!.click();
		await settle();
		expect(surfaceOpen()).toBe(false);
		expect(document.activeElement).not.toBe(anchor);
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

	it('routes a legacy PANEL event by the same address rule', async () => {
		mountHost(propsA);
		mountHost(propsB);
		notifyAttachmentPanelOpen(panelEvent({ hostToken: 'host-2' }));
		await settle();
		expect(surfaces()).toHaveLength(1);
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

	it('a legacy-panel single open whose file 404s shows the inert overlay, not a close (T2b)', async () => {
		// The retired panel showed "no longer available" for a single file that turned
		// out to be gone; the cutover must preserve that end-to-end. An incomplete
		// seed forces the probe, which 404s.
		metaFetch.mockResolvedValue({ status: 'missing' });
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent({ size_bytes: null }));
		await settle();
		// Still open — the surface did not flash-close.
		expect(surfaceOpen()).toBe(true);
		const missing = document.body.querySelector('.lightbox-missing');
		expect(missing).not.toBeNull();
		expect(missing?.textContent).toContain('no longer available');
	});

	it('closes a SINGLE-open surface when another surface deletes its attachment', async () => {
		mountHost(propsA);
		notifyAttachmentPanelOpen(panelEvent());
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
