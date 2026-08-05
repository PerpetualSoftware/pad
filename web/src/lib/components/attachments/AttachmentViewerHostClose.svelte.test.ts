import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

/**
 * The bound-close invariant, on its own because it needs `Lightbox` stubbed
 * (TASK-2428).
 *
 * `AttachmentViewerHost` hands each viewer a close handler BOUND to the request
 * it was rendered for, so a continuation from a viewer the host has already
 * destroyed cannot dismiss the one the user opened since. Driving that through
 * the real component is impossible in jsdom: the old close button is detached
 * by then, and a click on a detached node never reaches Svelte's delegated root
 * handler — a test written that way passes with the guard deleted (Codex round
 * 4). Holding the callback directly is the only honest way to fire it.
 */
vi.mock('$lib/components/common/Lightbox.svelte', async () => ({
	default: (await import('./fixtures/LightboxStub.svelte')).default,
}));

const { notifyViewerOpen } = await import('$lib/attachments/events');
const { lightboxStubCalls } = await import('./fixtures/lightboxStub');
const { default: AttachmentViewerHost } = await import('./AttachmentViewerHost.svelte');
type ViewerEvent = import('$lib/attachments/events').AttachmentViewerOpenEvent;

const ATT_ID = '11111111-2222-4333-8444-555555555555';
const ATT_ID_2 = '99999999-8888-4777-8666-555555555555';

function openEvent(over: Partial<ViewerEvent> = {}): ViewerEvent {
	return {
		attachmentId: ATT_ID,
		workspaceSlug: 'ws',
		itemId: 'item-a',
		hostToken: 'host-1',
		images: [
			{
				id: ATT_ID,
				alt: 'a diagram',
				filename: 'diagram.png',
				mime_type: 'image/png',
				size_bytes: 4096,
				width: 800,
				height: 600,
			},
		],
		index: 0,
		invoker: null,
		...over,
	};
}

function stub(): HTMLElement | null {
	return document.querySelector<HTMLElement>('.lightbox-stub');
}

const props = $state({ itemId: 'item-a' as string | null, hostToken: 'host-1', resourceGen: 1 });

describe('AttachmentViewerHost — bound close', () => {
	let target: HTMLElement;
	const mounted: ReturnType<typeof mount>[] = [];

	beforeEach(() => {
		lightboxStubCalls.length = 0;
		Object.assign(props, { itemId: 'item-a', hostToken: 'host-1', resourceGen: 1 });
		target = document.body.appendChild(document.createElement('div'));
		mounted.push(mount(AttachmentViewerHost, { target, props }));
		flushSync();
	});

	afterEach(() => {
		while (mounted.length) unmount(mounted.pop()!);
		target.remove();
	});

	it('remounts the viewer per open, rather than re-using one instance', () => {
		// Lightbox seeds its index once through `untrack`, so a re-used instance
		// would silently keep showing the first image.
		notifyViewerOpen(openEvent());
		flushSync();
		notifyViewerOpen(openEvent({ attachmentId: ATT_ID_2, images: [{ ...openEvent().images[0], id: ATT_ID_2 }] }));
		flushSync();

		expect(lightboxStubCalls).toHaveLength(2);
		expect(stub()?.dataset.attachmentId).toBe(ATT_ID_2);
	});

	it('a close from a DESTROYED viewer cannot dismiss the newer one', () => {
		notifyViewerOpen(openEvent());
		flushSync();
		const staleClose = lightboxStubCalls[0].onClose;

		notifyViewerOpen(openEvent({ attachmentId: ATT_ID_2, images: [{ ...openEvent().images[0], id: ATT_ID_2 }] }));
		flushSync();

		staleClose();
		flushSync();

		expect(stub()).not.toBeNull();
		expect(stub()?.dataset.attachmentId).toBe(ATT_ID_2);
	});

	it('the CURRENT viewer’s close still closes it', () => {
		// The guard must not be so broad that it makes closing impossible.
		notifyViewerOpen(openEvent());
		flushSync();
		lightboxStubCalls[0].onClose();
		flushSync();

		expect(stub()).toBeNull();
	});

	it('a close from a viewer torn down by a resource switch does not close the next one', () => {
		notifyViewerOpen(openEvent());
		flushSync();
		const staleClose = lightboxStubCalls[0].onClose;

		// The host switches item, destroying that viewer...
		props.itemId = 'item-b';
		props.resourceGen = 2;
		flushSync();
		expect(stub()).toBeNull();

		// ...and one opens on the new item.
		notifyViewerOpen(openEvent({ itemId: 'item-b', attachmentId: ATT_ID_2 }));
		flushSync();
		expect(stub()).not.toBeNull();

		staleClose();
		flushSync();
		expect(stub()).not.toBeNull();
	});

	it('returns focus to the invoker only on the bound close', () => {
		const invoker = target.appendChild(document.createElement('button'));
		const other = target.appendChild(document.createElement('button'));
		other.focus();

		notifyViewerOpen(openEvent({ invoker }));
		flushSync();
		lightboxStubCalls[0].onClose();
		flushSync();
		expect(document.activeElement).toBe(invoker);

		// A stale close returns nothing: it did not close anything, so moving
		// the user's focus would be a jump out of whatever they are now in.
		notifyViewerOpen(openEvent({ invoker }));
		flushSync();
		const staleClose = lightboxStubCalls[1].onClose;
		notifyViewerOpen(openEvent({ attachmentId: ATT_ID_2, invoker: null }));
		flushSync();
		other.focus();
		staleClose();
		flushSync();
		expect(document.activeElement).toBe(other);
	});
});
