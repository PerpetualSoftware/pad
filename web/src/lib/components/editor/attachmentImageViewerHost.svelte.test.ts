// The whole route: inline image NodeView → the REAL bus → the REAL
// `AttachmentViewerHost` → the REAL `Lightbox` (PLAN-2392 phase 3a / TASK-2433).
//
// Every other spec for this change stops at the producer: they mock
// `notifyViewerOpen` and assert the payload, which is the right shape for
// asking "did the NodeView emit the correct request" and is deliberately blind
// to whether anything is listening. That leaves the commit's actual headline
// claim — a deleted `<dialog>` REPLACED by the shared viewer, not removed —
// resting on two halves nobody joins. A host that stopped consuming, an
// address that could not route, a payload the viewer filters back out: all of
// them keep the producer specs green and leave the user with an image that
// does nothing.
//
// So nothing here is stubbed but the network. The bus is real, the host is
// real, `Lightbox` is real, and the assertion is a viewer in the document with
// the clicked attachment in it.
//
// What jsdom still cannot prove — real inertness, real Tab traversal, layout
// and stacking — is `Lightbox`'s own contract and belongs to the browser suite
// (TASK-2436), not to a jsdom test that would pass vacuously here.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';
import { _resetEscapeStackForTests } from '$lib/stores/escapeStack';
import {
	isAttachmentPanelEventForHost,
	registerAttachmentPanelListener,
	type AttachmentPanelOpenEvent,
} from '$lib/attachments/events';

const UUID = '11111111-1111-4111-8111-111111111111';
const ITEM_ID = 'item-A';
const HOST_TOKEN = 'apanel-1';

// The one stub: the HEAD probe. Everything the viewer path is made of stays
// real, because the point of this file is the wiring between the parts.
const probeMock = vi.fn(async () => ({ status: 'ok' as const, mime: 'image/png', size: 4096 }));
vi.mock('./attachment-metadata', () => ({
	fetchAttachmentMetadata: () => probeMock(),
	revalidateAttachmentMetadata: () => probeMock(),
	invalidateAttachmentMetadata: () => {},
	mimeToFormat: () => null,
}));

const { AttachmentImage } = await import('./attachment-image');
const { default: AttachmentViewerHost } = await import(
	'$lib/components/attachments/AttachmentViewerHost.svelte'
);
const { default: AttachmentPanelHost } = await import(
	'$lib/components/attachments/AttachmentPanelHost.svelte'
);

let address = { workspaceSlug: 'ws', itemId: ITEM_ID, hostToken: HOST_TOKEN };

function makeEditor(element: HTMLElement): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentImage.configure({
				workspaceSlug: 'ws',
				getDownloadUrl: (uuid: string, variant?: string) =>
					`/api/v1/workspaces/ws/attachments/${uuid}?variant=${variant ?? 'thumb-md'}`,
				address: () => address,
				supportedFormats: ['png'],
				transform: async () => {
					throw new Error('not used');
				},
			}),
		],
		content: `<p><img data-attachment-id="${UUID}" src="/api/v1/x" alt="A diagram"></p>`,
		editable: true,
	});
}

describe('inline image → viewer host → Lightbox', () => {
	let editorTarget: HTMLElement;
	let hostTarget: HTMLElement;
	let editor: Editor | undefined;
	let host: Record<string, unknown> | null = null;

	function mountHost(props: { itemId: string; hostToken: string }) {
		host = mount(AttachmentViewerHost, { target: hostTarget, props }) as Record<string, unknown>;
		flushSync();
	}

	/** Viewers actually on screen — the real component's root. */
	function viewers(): HTMLElement[] {
		return Array.from(document.body.querySelectorAll<HTMLElement>('.lightbox-backdrop'));
	}

	function image(): HTMLImageElement {
		const el = editorTarget.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!el) throw new Error('image NodeView did not render');
		return el;
	}

	/** Activation resolves the MIME before emitting, so it spans a task. */
	async function settle() {
		await new Promise((resolve) => setTimeout(resolve, 0));
		await new Promise((resolve) => setTimeout(resolve, 0));
		flushSync();
	}

	beforeEach(() => {
		_resetEscapeStackForTests();
		__resetViewerBackdropForTests();
		address = { workspaceSlug: 'ws', itemId: ITEM_ID, hostToken: HOST_TOKEN };
		probeMock.mockClear();
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 4096 });
		editorTarget = document.body.appendChild(document.createElement('div'));
		hostTarget = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		editor?.destroy();
		editor = undefined;
		if (host) unmount(host);
		host = null;
		editorTarget.remove();
		hostTarget.remove();
		document.body.querySelectorAll('.lightbox-backdrop').forEach((el) => el.remove());
	});

	// Run against TWO different addresses. One fixed pair cannot tell a producer
	// that STAMPS the address from one that hard-codes the constants this file
	// happens to use — and a hard-coded address is exactly what DR-8 exists to
	// prevent, since it would route every editor's images at one pane.
	for (const [label, addr] of [
		['the default address', { itemId: ITEM_ID, hostToken: HOST_TOKEN }],
		['a different item and mount', { itemId: 'item-Z', hostToken: 'apanel-9' }],
	] as const) {
		it(`opens the shared viewer on the image the user activated — ${label}`, async () => {
			address = { workspaceSlug: 'ws', ...addr };
			mountHost(addr);
			editor = makeEditor(editorTarget);
			expect(viewers()).toHaveLength(0);

			image().dispatchEvent(
				new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 })
			);
			await settle();

			// ONE viewer, and it is the real component: a `role="dialog"` root
			// portaled to `<body>`, showing the attachment that was clicked. The
			// deleted `<dialog>` is not merely gone — something took its place.
			expect(viewers()).toHaveLength(1);
			const viewer = viewers()[0];
			expect(viewer.getAttribute('role')).toBe('dialog');
			expect(viewer.parentElement).toBe(document.body);
			const shown = viewer.querySelector<HTMLImageElement>('img.lightbox-image');
			expect(shown?.getAttribute('src')).toContain(UUID);
			// The full-resolution blob, as the deleted dialog loaded: the viewer
			// asks for the canonical URL with no `variant`.
			expect(shown?.getAttribute('src')).not.toContain('variant=');
		});
	}

	it('opens on the KEYBOARD too, exactly once', async () => {
		// The route the deleted dialog never had. A count, so a second emitter
		// anywhere along the chain is visible.
		mountHost({ itemId: ITEM_ID, hostToken: HOST_TOKEN });
		editor = makeEditor(editorTarget);

		image().dispatchEvent(
			new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })
		);
		await settle();

		expect(viewers()).toHaveLength(1);
	});

	it('reaches only the host it is ADDRESSED to', async () => {
		// DR-8's rule, asserted end to end rather than on the channel alone: a
		// master pane and a peeked pane are both mounted, and a NodeView's event
		// must open the viewer over the one that owns it. A producer that
		// stamped a constant, or a host that consumed everything, both show up
		// here as a viewer in the wrong place.
		mountHost({ itemId: ITEM_ID, hostToken: 'a-different-mount' });
		editor = makeEditor(editorTarget);

		image().dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		expect(viewers()).toHaveLength(0);
	});

	it('opens nothing at all when no host is mounted', async () => {
		// The honest statement of what this surface now depends on: an editor
		// outside an `ItemDetail` announces a button and, with nobody listening,
		// opens nothing. Every real mount threads a token (both `<Editor>`s and
		// every `CommentEditor` come from `ItemDetail`), so this is the
		// boundary, not a live bug — pinned so a future reusable-editor surface
		// discovers it here rather than as a dead control in front of a user.
		editor = makeEditor(editorTarget);

		image().dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		expect(viewers()).toHaveLength(0);
	});

	it('REDIRECTS a non-allowlisted type into the real panel host', async () => {
		// The whole route for the redirect arm, with nothing between the NodeView
		// and the panel stubbed: the real bus, the real `AttachmentPanelHost`, the
		// real panel it mounts. The producer specs next door mock the bus, so a
		// host that stopped consuming — or an address that could not route —
		// would leave them green and leave the user with an image that does
		// nothing, which is the exact failure this task replaced.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 100 } as never);
		mountHost({ itemId: ITEM_ID, hostToken: HOST_TOKEN });
		const panelHost = mount(AttachmentPanelHost, {
			target: hostTarget,
			props: {
				wsSlug: 'ws',
				itemId: ITEM_ID,
				hostToken: HOST_TOKEN,
				mutationsEnabled: false,
			},
		}) as Record<string, unknown>;
		try {
			editor = makeEditor(editorTarget);

			image().dispatchEvent(
				new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 })
			);
			await settle();

			// No viewer — the security half.
			expect(viewers()).toHaveLength(0);
			// And a real panel on screen, for the attachment that was activated —
			// the completeness half. A redirect nobody consumes is still a tap
			// that does nothing.
			const panel = document.body.querySelector<HTMLElement>('[role="menu"] .ap-header');
			expect(panel).not.toBeNull();
			// And it is about THIS attachment, not merely present: the MIME the
			// probe returned and a download target carrying the uuid. A panel that
			// opened on the wrong row would satisfy a presence check.
			expect(panel?.querySelector('.ap-meta')?.getAttribute('title')).toBe('image/svg+xml');
			expect(
				document.body.querySelector<HTMLAnchorElement>('[role="menu"] a[download]')?.getAttribute('href')
			).toContain(UUID);
		} finally {
			unmount(panelHost);
		}
	});

	it('REDIRECTS a non-allowlisted type onto the real panel channel', async () => {
		// TASK-2434's redirect, asserted through the REAL bus rather than a mock.
		// The producer specs next door mock `notifyAttachmentPanelOpen`, so they
		// see the call and are blind to what the channel does with it — and the
		// channel drops any emission it judges unaddressable. An event that never
		// leaves the bus is indistinguishable, from the producer's side, from the
		// silent refusal this task replaced.
		const seen: AttachmentPanelOpenEvent[] = [];
		const dispose = registerAttachmentPanelListener((e) => seen.push(e));
		try {
			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 100 } as never);
			mountHost({ itemId: ITEM_ID, hostToken: HOST_TOKEN });
			editor = makeEditor(editorTarget);

			image().dispatchEvent(
				new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 })
			);
			await settle();

			// No viewer — the security half.
			expect(viewers()).toHaveLength(0);
			// And it went SOMEWHERE — the completeness half.
			expect(seen).toHaveLength(1);
			expect(seen[0].attachmentId).toBe(UUID);
			expect(seen[0].mime_type).toBe('image/svg+xml');
			// Addressed well enough for a host to claim it. The channel's own
			// predicate, not a re-implementation of it: a payload that reached a
			// raw subscriber but that no host would match is still a tap that
			// does nothing.
			expect(
				isAttachmentPanelEventForHost(seen[0], { itemId: ITEM_ID, hostToken: HOST_TOKEN })
			).toBe(true);
			expect(
				isAttachmentPanelEventForHost(seen[0], { itemId: ITEM_ID, hostToken: 'another-mount' })
			).toBe(false);
		} finally {
			dispose();
		}
	});

	it('does not open for a MIME the viewer would filter back out', async () => {
		// The producer's gate and the viewer's are the same gate, stated twice
		// on purpose (TASK-2431). If the producer ever emitted an SVG, the
		// viewer would filter it and mount an empty shell — the failure this
		// asserts is absent.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 100 });
		mountHost({ itemId: ITEM_ID, hostToken: HOST_TOKEN });
		editor = makeEditor(editorTarget);

		image().dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		expect(viewers()).toHaveLength(0);
	});
});
