import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

// TASK-2428. The viewer is exercised THROUGH its host, because the host is
// where the two rules that matter live: an event is consumed only when both
// `itemId` and `hostToken` are this host's own (DR-8), and an open viewer is
// closed by a RESOURCE SWITCH and by nothing else. Nothing routes into it yet —
// the emitters land in the next task — so these tests emit on the bus directly,
// which is also the only way to drive a NodeView-originated open.
//
// The bus stays REAL: addressing is the thing under test.
//
// What jsdom CANNOT prove here, and is therefore the browser suite's: focus
// entry, background inertness, and the viewer's real stacking against a panel
// or a menu opened at the same time.

// The bus stays REAL, with ONE wrapper: the registration is counted and its
// disposer is tracked, because "an unmounted host stops receiving events" is
// otherwise untestable through the DOM — a destroyed component renders nothing
// whether or not its listener leaked, so a leak would pass silently.
const subs = vi.hoisted(() => ({ registered: 0, disposed: 0 }));
vi.mock('$lib/attachments/events', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/attachments/events')>();
	return {
		...actual,
		registerAttachmentViewerListener: (fn: Parameters<
			typeof actual.registerAttachmentViewerListener
		>[0]) => {
			subs.registered += 1;
			const off = actual.registerAttachmentViewerListener(fn);
			return () => {
				subs.disposed += 1;
				off();
			};
		},
	};
});

const { notifyViewerOpen } = await import('$lib/attachments/events');
type ViewerEvent = import('$lib/attachments/events').AttachmentViewerOpenEvent;
type LightboxImage = import('$lib/attachments/events').LightboxImage;
const { default: AttachmentViewerHost } = await import('./AttachmentViewerHost.svelte');

const ATT_ID = '11111111-2222-4333-8444-555555555555';
const ATT_ID_2 = '99999999-8888-4777-8666-555555555555';

function image(over: Partial<LightboxImage> = {}): LightboxImage {
	return {
		id: ATT_ID,
		alt: 'a diagram',
		filename: 'diagram.png',
		mime_type: 'image/png',
		size_bytes: 4096,
		width: 800,
		height: 600,
		...over,
	};
}

function openEvent(over: Partial<ViewerEvent> = {}): ViewerEvent {
	return {
		attachmentId: ATT_ID,
		workspaceSlug: 'ws',
		itemId: 'item-a',
		hostToken: 'host-1',
		images: [image()],
		index: 0,
		invoker: null,
		...over,
	};
}

/**
 * The viewer is a fixed-position overlay rendered into its host's own mount
 * container, which is what lets a two-host test say WHICH host opened rather
 * than only how many viewers exist.
 */
function viewers(scope: ParentNode = document): HTMLElement[] {
	return Array.from(scope.querySelectorAll<HTMLElement>('.lightbox-backdrop'));
}

function viewer(): HTMLElement | null {
	return viewers()[0] ?? null;
}

function viewerImage(): HTMLImageElement | null {
	return document.querySelector<HTMLImageElement>('.lightbox-image');
}

interface HostProps {
	itemId: string | null;
	hostToken: string;
	resourceGen: number;
}

// Two reactive props objects, declared at the top level because `$state(...)`
// may only initialize a declaration. Two of them because the pane host runs a
// master and a peeked ItemDetail at once, which is exactly what DR-8's
// addressing exists for.
const propsA = $state<HostProps>({ itemId: 'item-a', hostToken: 'host-1', resourceGen: 1 });
const propsB = $state<HostProps>({ itemId: 'item-a', hostToken: 'host-2', resourceGen: 1 });

describe('AttachmentViewerHost', () => {
	let target: HTMLElement;
	const mounted: ReturnType<typeof mount>[] = [];

	beforeEach(() => {
		subs.registered = 0;
		subs.disposed = 0;
		Object.assign(propsA, { itemId: 'item-a', hostToken: 'host-1', resourceGen: 1 });
		Object.assign(propsB, { itemId: 'item-a', hostToken: 'host-2', resourceGen: 1 });
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		while (mounted.length) unmount(mounted.pop()!);
		target.remove();
		document.querySelectorAll('.viewer-host-target').forEach((el) => el.remove());
	});

	/** Mounts a host in its OWN container, and hands that container back. */
	function mountHost(props: HostProps): HTMLElement {
		const container = target.appendChild(document.createElement('div'));
		container.className = 'viewer-host-target';
		mounted.push(mount(AttachmentViewerHost, { target: container, props }));
		flushSync();
		return container;
	}

	it('opens for an event addressed to it, on the event’s workspace', () => {
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();

		expect(viewer()).not.toBeNull();
		// The workspace is the one CAPTURED at emit, not one the host resolved:
		// the pane switches workspace without remounting.
		expect(viewerImage()?.getAttribute('src')).toContain('/workspaces/ws/attachments/');
		expect(viewerImage()?.getAttribute('alt')).toBe('a diagram');
	});

	it('serves the emit-time workspace even when it is not the host’s current one', () => {
		mountHost(propsA);
		notifyViewerOpen(openEvent({ workspaceSlug: 'other-ws' }));
		flushSync();

		expect(viewerImage()?.getAttribute('src')).toContain('/workspaces/other-ws/attachments/');
	});

	it('ignores an event addressed to the OTHER host, with both mounted', () => {
		const containerA = mountHost(propsA);
		const containerB = mountHost(propsB);

		// Same item, other host token: exactly one viewer may open, and it must
		// be the ADDRESSED one — counting viewers alone would pass if the wrong
		// host opened. Matching on itemId alone would open two.
		notifyViewerOpen(openEvent({ hostToken: 'host-2' }));
		flushSync();

		expect(viewers()).toHaveLength(1);
		expect(viewers(containerB)).toHaveLength(1);
		expect(viewers(containerA)).toHaveLength(0);

		// ...and the reverse direction, so neither host is simply inert.
		notifyViewerOpen(openEvent({ hostToken: 'host-1' }));
		flushSync();
		expect(viewers(containerA)).toHaveLength(1);
		expect(viewers()).toHaveLength(2);
	});

	it('ignores an event for a different item on its own token', () => {
		mountHost(propsA);
		notifyViewerOpen(openEvent({ itemId: 'item-b' }));
		flushSync();

		expect(viewer()).toBeNull();
	});

	it('opens at the requested index and pages through the whole set', () => {
		mountHost(propsA);
		notifyViewerOpen(
			openEvent({
				attachmentId: ATT_ID_2,
				images: [image(), image({ id: ATT_ID_2, alt: 'second' })],
				index: 1,
			})
		);
		flushSync();

		expect(viewerImage()?.getAttribute('src')).toContain(ATT_ID_2);
		document.querySelector<HTMLButtonElement>('.lightbox-nav.prev')!.click();
		flushSync();
		expect(viewerImage()?.getAttribute('src')).toContain(ATT_ID);
	});

	it('REMOUNTS on a second open, so the new index is honoured', () => {
		// Lightbox seeds `current` once through `untrack`, so a prop update
		// would leave the second open showing the first image.
		mountHost(propsA);
		const images = [image(), image({ id: ATT_ID_2, alt: 'second' })];
		notifyViewerOpen(openEvent({ images, index: 0 }));
		flushSync();
		expect(viewerImage()?.getAttribute('src')).toContain(ATT_ID);

		notifyViewerOpen(openEvent({ attachmentId: ATT_ID_2, images, index: 1 }));
		flushSync();
		expect(viewers()).toHaveLength(1);
		expect(viewerImage()?.getAttribute('src')).toContain(ATT_ID_2);
	});

	it('closes on the viewer’s own close control', () => {
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();
		document.querySelector<HTMLButtonElement>('.lightbox-close')!.click();
		flushSync();

		expect(viewer()).toBeNull();
	});

	it('returns focus to the invoker on close', () => {
		const invoker = target.appendChild(document.createElement('button'));
		mountHost(propsA);
		notifyViewerOpen(openEvent({ invoker }));
		flushSync();
		document.querySelector<HTMLButtonElement>('.lightbox-close')!.click();
		flushSync();

		expect(document.activeElement).toBe(invoker);
	});

	it('does not throw when the invoker is gone by the time the viewer closes', () => {
		// An editor NodeView is re-rendered on any document change, so the
		// element that opened the viewer can be detached by now.
		const invoker = target.appendChild(document.createElement('button'));
		mountHost(propsA);
		notifyViewerOpen(openEvent({ invoker }));
		flushSync();
		invoker.remove();
		expect(() =>
			document.querySelector<HTMLButtonElement>('.lightbox-close')!.click()
		).not.toThrow();
		flushSync();
		expect(viewer()).toBeNull();
	});

	it('closes when the host switches item', () => {
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewer()).not.toBeNull();

		propsA.itemId = 'item-b';
		flushSync();
		expect(viewer()).toBeNull();
	});

	it('closes when the loaded resource changes under a stable item id', () => {
		// The generation is the arm that survives a mid-switch where the id
		// prop never visibly settles on a different value.
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();

		propsA.resourceGen = 2;
		flushSync();
		expect(viewer()).toBeNull();
	});

	it('does NOT close on a same-resource refresh', () => {
		// The whole reason for a dedicated generation: ItemDetail's
		// `loadGeneration` is bumped by every loadData(), including the
		// same-item reload after a collection schema edit. Keying on that would
		// tear the viewer down on a refresh that changed nothing it can see.
		//
		// This is the shape a refresh has at this boundary: `loadData()` does
		// not null `item`, and `itemMatchesRef` ignores the collection and
		// username segments, so both a reload and a collection-only route
		// change leave the id AND the generation exactly where they were —
		// re-asserted, never absent. (The `loading` flip that used to destroy
		// this host outright is why it is mounted at ItemDetail's top level.)
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();

		propsA.itemId = 'item-a';
		propsA.resourceGen = 1;
		flushSync();

		expect(viewer()).not.toBeNull();
		expect(viewerImage()?.getAttribute('src')).toContain(ATT_ID);
	});

	it('closes when the item goes away without another arriving', () => {
		// The id is `itemMatchesRef ? item?.id : null`, so null means the item
		// this viewer belongs to is no longer the one on screen. Waiting for the
		// next non-empty id instead would strand the viewer over a skeleton or
		// an error page whenever the incoming item never loads.
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewer()).not.toBeNull();

		propsA.itemId = null;
		flushSync();
		expect(viewer()).toBeNull();
	});

	it('opens normally on a host that mounted with an already-loaded resource', () => {
		// The lifecycle rule is about TRANSITIONS. A host seeded from its
		// initial props has nothing to clear, and must not swallow the first
		// event because its own mount looked like a change.
		mountHost(propsA);
		flushSync();
		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewer()).not.toBeNull();
	});

	it('unsubscribes from the bus on unmount, so a dead host receives nothing', () => {
		// Asserted through the disposer, not the DOM: a destroyed component
		// renders nothing whether or not its listener leaked, so a DOM-only
		// check here would pass with the subscription still live and firing
		// into a dead view for the rest of the session.
		const before = subs.registered;
		mountHost(propsA);
		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewer()).not.toBeNull();
		expect(subs.registered).toBe(before + 1);
		expect(subs.disposed).toBe(0);

		unmount(mounted.pop()!);
		flushSync();
		expect(subs.disposed).toBe(1);

		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewer()).toBeNull();
	});

	it('subscribes ONCE and still addresses on its CURRENT item after a switch', () => {
		// The registration effect reads `itemId` / `hostToken` inside the
		// callback at EMIT time rather than in its tracked scope. Reading them
		// outside would both re-run the effect on every item switch (tearing the
		// listener down and re-adding it) AND capture the address, so the host
		// would keep answering for the item it mounted on. Counting
		// registrations catches the first half; the emissions below catch the
		// second, which is the one that silently breaks a tap after a switch.
		const before = subs.registered;
		mountHost(propsA);
		propsA.itemId = 'item-b';
		propsA.resourceGen = 2;
		flushSync();

		// The address it mounted with is now stale and must be ignored...
		notifyViewerOpen(openEvent({ itemId: 'item-a' }));
		flushSync();
		expect(viewer()).toBeNull();

		// ...while the item it is showing NOW is answered.
		notifyViewerOpen(openEvent({ itemId: 'item-b' }));
		flushSync();
		expect(viewer()).not.toBeNull();

		expect(subs.registered).toBe(before + 1);
		expect(subs.disposed).toBe(0);
	});

	it('keeps the flush alive: a teardown does not strand neighbouring reactivity', () => {
		// The self-write hazard (CONVE-1688): an $effect that writes a $state it
		// also reads in its tracked scope ABORTS the flush, which strands
		// unrelated reactivity elsewhere in the same flush and reports nothing
		// in a production build. A test that only checks this host's own
		// counter can pass while the component is quietly breaking its
		// neighbours, so the assertion has to be about a NEIGHBOUR.
		//
		// Two hosts driven by ONE props object: they share a flush, so if the
		// first one's lifecycle effect aborts it, the second's teardown never
		// runs and its viewer stays on screen. Exercised open → mutate →
		// teardown → RE-open → mutate, because a hazard that no-ops on its
		// first write only shows once the state has actually moved.
		const first = mountHost(propsA);
		const neighbour = mountHost(propsA);

		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewers(first)).toHaveLength(1);
		expect(viewers(neighbour)).toHaveLength(1);

		propsA.resourceGen = 2;
		flushSync();
		expect(viewers(first)).toHaveLength(0);
		expect(viewers(neighbour)).toHaveLength(0);

		// Re-open on the same (now current) resource and drive it again.
		notifyViewerOpen(openEvent());
		flushSync();
		expect(viewers(neighbour)).toHaveLength(1);

		propsA.itemId = 'item-b';
		flushSync();
		expect(viewers(first)).toHaveLength(0);
		expect(viewers(neighbour)).toHaveLength(0);

		// And the neighbour is still LIVE, not merely emptied: it answers the
		// new address, which it could not do if its effects had been stranded.
		notifyViewerOpen(openEvent({ itemId: 'item-b' }));
		flushSync();
		expect(viewers(neighbour)).toHaveLength(1);
	});

	// The bound-close invariant is NOT tested here: driving it needs the close
	// callback of a viewer the host has already destroyed, and a click on the
	// detached button never reaches Svelte's delegated root handler — the
	// version of this test written that way passed with the guard deleted.
	// It lives in `AttachmentViewerHostClose.svelte.test.ts`, against a stubbed
	// Lightbox that hands the callback back.
});
