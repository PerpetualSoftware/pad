// The inline image's missing-attachment placeholder (PLAN-2392 DR-12 / DR-17).
//
// Two causes land on the same element and must NOT look the same to a keyboard
// or screen-reader user:
//
//   - a transient load failure — retryable, so the placeholder is a button;
//   - a CONFIRMED deletion — `retryLoad` refuses, so a button that announces
//     itself and does nothing is a dead focus stop. That is the same failure
//     the file chip's `disabled` closes, on the surface right next to it.
//
// Driven through a REAL Tiptap editor, like the chip spec: the placeholder is
// imperative NodeView DOM and its accessibility semantics are properties of
// that DOM, which a hand-built element would not pin.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';

const deletionListeners = new Set<(uuid: string) => void>();
vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentPanelOpen: () => {},
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
}));

// No probe: these tests are about what the placeholder IS, not how it is
// discovered, and a real HEAD would make them asynchronous for no gain.
const probeMock = vi.fn(async () => ({ status: 'transient' as const }));
vi.mock('./attachment-metadata', () => ({
	fetchAttachmentMetadata: () => probeMock(),
	revalidateAttachmentMetadata: () => probeMock(),
	invalidateAttachmentMetadata: () => {},
	mimeToFormat: () => null,
}));

const { AttachmentImage } = await import('./attachment-image');

function makeEditor(element: HTMLElement): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentImage.configure({
				workspaceSlug: '',
				getDownloadUrl: (uuid: string) => `/api/v1/workspaces/ws/attachments/${uuid}`,
				// A workspace IS supplied: the MIME probe is what the viewer gate
				// reads, and with an empty one it never runs (which is itself the
				// documented "unknown ⇒ keep today's behaviour" path).
				address: () => ({ workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' }),
				supportedFormats: [],
				transform: async () => {
					throw new Error('not used');
				},
			}),
		],
		content: '<p><img data-attachment-id="uuid-1" src="/api/v1/x" alt="A diagram"></p>',
		editable: true,
	});
}

describe('inline image missing placeholder', () => {
	let target: HTMLElement;
	let editor: Editor | undefined;

	beforeEach(() => {
		deletionListeners.clear();
		probeMock.mockClear();
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		editor?.destroy();
		editor = undefined;
		target.remove();
	});

	function placeholder(): HTMLElement {
		editor ??= makeEditor(target);
		const el = target.querySelector<HTMLElement>('.attachment-missing');
		if (!el) throw new Error('placeholder did not render');
		return el;
	}

	function failLoad() {
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');
		img.dispatchEvent(new Event('error'));
	}

	it('refuses to open a probed non-raster type in the viewer (DR-16)', async () => {
		// The allowlist gates EVERY open-the-viewer path, not just the strip's:
		// image/svg+xml can carry active content, and a node being labelled
		// image/* is not sufficient reason to hand it to a viewer.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml' });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		// Select the node so the lazy MIME probe runs, then let it settle.
		editor.commands.setNodeSelection(1);
		await Promise.resolve();
		await Promise.resolve();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		expect(document.querySelector('dialog.attachment-image-lightbox')).toBeNull();
	});

	it('still opens an allowlisted raster type', async () => {
		// The gate must not cost the common case: a PNG opens as it always did.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png' });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		editor.commands.setNodeSelection(1);
		await Promise.resolve();
		await Promise.resolve();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		expect(document.querySelector('dialog.attachment-image-lightbox')).not.toBeNull();
		document.querySelector('dialog.attachment-image-lightbox')?.remove();
	});

	it('is a focusable button while the failure is merely transient', () => {
		editor = makeEditor(target);
		failLoad();

		const el = placeholder();
		expect(el.style.display).not.toBe('none');
		// Retryable, so it invites the retry and can be reached to perform it.
		expect(el.getAttribute('role')).toBe('button');
		expect(el.getAttribute('tabindex')).toBe('0');
		expect(el.title).toContain('retry');
	});

	it('drops its interactive semantics once the deletion is confirmed', async () => {
		editor = makeEditor(target);
		failLoad();
		expect(placeholder().getAttribute('role')).toBe('button');

		// Another surface deleted the row: authoritative, and `retryLoad`
		// refuses from here on.
		for (const fn of deletionListeners) fn('uuid-1');

		const el = placeholder();
		expect(el.getAttribute('role')).toBeNull();
		expect(el.getAttribute('tabindex')).toBeNull();
		// The copy stops inviting a retry that cannot happen, too.
		expect(el.title).toBe('This attachment has been deleted');
		expect(el.title).not.toContain('retry');
	});

	it('does not leave focus stranded on a placeholder that just went inert', () => {
		editor = makeEditor(target);
		failLoad();
		const el = placeholder();
		el.focus();
		expect(document.activeElement).toBe(el);

		for (const fn of deletionListeners) fn('uuid-1');

		// Removing tabindex from the focused element would otherwise leave
		// focus on something unreachable by any further keystroke.
		expect(document.activeElement).not.toBe(el);
	});
});
