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
	isAttachmentSurfaceEventForHost,
	registerAttachmentSurfaceListener,
	type AttachmentSurfaceOpenEvent,
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
// 3c-ii T2b: the two hosts collapsed into the ONE `AttachmentSurfaceHost`, which
// bridges the legacy viewer AND panel channels. This whole-route test mounts it
// instead — the raster arm for an image, the fallback arm for the SVG redirect.
const { default: AttachmentSurfaceHost } = await import(
	'$lib/components/attachments/AttachmentSurfaceHost.svelte'
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
				supportedFormats: () => ['png'],
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
		host = mount(AttachmentSurfaceHost, {
			target: hostTarget,
			props: { wsSlug: 'ws', ...props },
		}) as Record<string, unknown>;
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
			// The URL is built from the CAPTURED workspace this NodeView emitted in
			// (the pane can switch workspace under a mounted viewer) — this pins
			// which workspace, which is why it survives the DR-5b loading rework.
			expect(shown?.getAttribute('src')).toContain(UUID);
			// NO `?variant`: an inline editor image carries no dimensions, so it is
			// `unknown` to the DR-5b classifier, and the unknown-dims desktop cell
			// requests the ORIGINAL directly — the canonical, no-variant URL — never
			// a thumbnail (TASK-2459). A dimensioned large image WOULD ship
			// `?variant=thumb-md` first; that path is covered in the loader tests.
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

	it('REDIRECTS a non-allowlisted type onto the FALLBACK arm of the one surface (3c-ii)', async () => {
		// The whole route for the redirect arm, with nothing between the NodeView
		// and the surface stubbed. Pre-convergence this opened a separate panel host;
		// now the ONE `AttachmentSurfaceHost` consumes the same panel channel and the
		// file-capable `Lightbox` opens the SVG on its NO-BYTES fallback arm — a real
		// surface on screen (the completeness half), no raster `<img>` and no request
		// (the security half). A host that stopped consuming would leave the producer
		// specs next door green and the user with a tap that does nothing.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 100 } as never);
		mountHost({ itemId: ITEM_ID, hostToken: HOST_TOKEN });
		editor = makeEditor(editorTarget);

		image().dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		// One surface, and it is the fallback arm: no raster bytes for the SVG.
		expect(viewers()).toHaveLength(1);
		expect(document.body.querySelector('.lightbox-image')).toBeNull();
		expect(document.body.querySelector('.lightbox-fallback')).not.toBeNull();
		// And it is about THIS attachment: a download target carrying the uuid.
		expect(
			document.body
				.querySelector<HTMLAnchorElement>('.lightbox-toolbar [aria-label="Download"]')
				?.getAttribute('href')
		).toContain(UUID);
	});

	it('emits the svg onto the SURFACE channel, addressably (producer contract)', async () => {
		// TASK-2489's convergence, asserted through the REAL bus rather than a mock.
		// The redirect distinction is gone: the NodeView no longer picks a panel
		// vs viewer channel by MIME — it emits ONE surface event carrying the true
		// svg MIME, and the surface's own renderer (the previous test, which mounts
		// the host) draws it on the no-bytes fallback arm. The producer specs next
		// door mock the notifier, so they see the call and are blind to what the
		// channel does with it — and the channel drops any emission it judges
		// unaddressable. An event that never leaves the bus is indistinguishable,
		// from the producer's side, from a silent refusal.
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const dispose = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 100 } as never);
			// No host mounted: this pins the PRODUCER contract (the NodeView emits an
			// SVG onto the surface channel, addressably), independent of who consumes
			// it. The host consuming it into the fallback arm is the previous test.
			editor = makeEditor(editorTarget);

			image().dispatchEvent(
				new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 })
			);
			await settle();

			// No surface — nothing is mounted to consume the channel here.
			expect(viewers()).toHaveLength(0);
			// And it went SOMEWHERE — the completeness half.
			expect(seen).toHaveLength(1);
			expect(seen[0].attachmentId).toBe(UUID);
			// The true svg MIME rides through, both as the flat seed and on the image.
			expect(seen[0].mime_type).toBe('image/svg+xml');
			expect(seen[0].images[0].mime_type).toBe('image/svg+xml');
			// Addressed well enough for a host to claim it. The channel's own
			// predicate, not a re-implementation of it: a payload that reached a
			// raw subscriber but that no host would match is still a tap that
			// does nothing.
			expect(
				isAttachmentSurfaceEventForHost(seen[0], { itemId: ITEM_ID, hostToken: HOST_TOKEN })
			).toBe(true);
			expect(
				isAttachmentSurfaceEventForHost(seen[0], { itemId: ITEM_ID, hostToken: 'another-mount' })
			).toBe(false);
		} finally {
			dispose();
		}
	});

	// (3c-ii T2b) The old "does not open for a MIME the viewer would filter back
	// out" test is RETIRED: it asserted an SVG opened NOTHING, which held only while
	// the surface was image-only. The converged surface admits an SVG and draws it
	// on the no-bytes FALLBACK arm — the security guarantee moved from "no surface"
	// to "no raster bytes", and the redirect test above pins exactly that (a surface
	// present, `.lightbox-image` absent, `.lightbox-fallback` present).
});
