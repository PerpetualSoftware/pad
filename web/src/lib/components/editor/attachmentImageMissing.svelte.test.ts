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
//
// The surface assertions here are PRODUCER-level — `notifyAttachmentSurfaceOpen`
// is mocked, so "opens" means "asks". A broken or absent host would be invisible
// to them; that half of the route is `attachmentImageViewerHost.svelte.test.ts`'s.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';

const deletionListeners = new Set<(uuid: string) => void>();
// Every open-the-viewer request this NodeView makes, captured RAW — before the
// channel's own addressability filter. The gate under test is the NodeView's;
// routing is `events.ts`'s and has its own spec.
const emitted: Array<Record<string, unknown>> = [];
vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentSurfaceOpen: (event: Record<string, unknown>) => {
		emitted.push(event);
	},
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
}));

// The probe's full result union, spelled out. Without it `vi.fn` infers the
// type of the DEFAULT implementation alone, and every `mockResolvedValue` for a
// different arm is a type error — invisible under `npm run check`, which
// excludes `*.svelte.test.ts`, and a trap for whoever widens that exclude.
type ProbeResult =
	| { status: 'ok'; mime: string; size: number }
	| { status: 'missing' }
	| { status: 'transient' };

// No probe by default: these tests are about what the placeholder IS, not how
// it is discovered, and a real HEAD would make them asynchronous for no gain.
const probeMock = vi.fn<() => Promise<ProbeResult>>(async () => ({ status: 'transient' }));
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
				supportedFormats: () => [],
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
		emitted.length = 0;
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

	/**
	 * Let the lazy HEAD probe AND the activation's own MIME resolution settle.
	 * Activation is asynchronous as of TASK-2433 — it resolves the MIME before
	 * emitting — so a synchronous assertion after a click would read `[]` no
	 * matter what the implementation does.
	 */
	async function settle() {
		await new Promise((resolve) => setTimeout(resolve, 0));
	}

	function failLoad() {
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');
		img.dispatchEvent(new Event('error'));
	}

	it('emits a surface event for a probed non-raster type — the surface, not the producer, picks the fallback arm (T4a)', async () => {
		// As of TASK-2489 the raster/non-raster fork is GONE from the producer.
		// The DR-16 guarantee — an `image/svg+xml` (active content) is never
		// handed to a raster viewer — did not disappear; it moved DOWNSTREAM to
		// the surface renderer, which routes svg onto its no-bytes fallback arm.
		// So the producer emits ONE surface event carrying the TRUE svg MIME, and
		// admission no longer branches on the type here. The resolve-before-emit
		// gate (missing → latch, transient → retry) is unchanged.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml' , size: 4096 });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		// Select the node so the lazy MIME probe runs, then let it settle.
		editor.commands.setNodeSelection(1);
		await settle();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();
		// Exactly ONE surface emit, stamped with the real svg MIME — the surface
		// renderer, not the producer, decides it renders no bytes.
		expect(emitted).toEqual([
			{
				attachmentId: 'uuid-1',
				workspaceSlug: 'ws',
				itemId: 'item-A',
				hostToken: 'apanel-1',
				images: [
					{
						id: 'uuid-1',
						alt: 'A diagram',
						filename: null,
						mime_type: 'image/svg+xml',
						size_bytes: 4096,
						width: null,
						height: null,
					},
				],
				index: 0,
				invoker: img,
				filename: null,
				mime_type: 'image/svg+xml',
				size_bytes: 4096,
			},
		]);
	});

	it('emits a fully-stamped surface request for an allowlisted raster type', async () => {
		// The gate must not cost the common case: a PNG opens as it always did —
		// and this is the assertion the deletion of the old `<dialog>` rests on.
		// "The dialog is gone" is satisfied by an implementation that opens
		// NOTHING, so the claim has to be about what is EMITTED.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 4096 });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		editor.commands.setNodeSelection(1);
		await settle();

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		// The WHOLE payload, field by field. Each one is load-bearing and each
		// has a plausible wrong value: the address routes the event to one of
		// several mounted hosts (DR-8), `workspaceSlug` is what every image URL
		// is read from, `mime_type` is what lets `Lightbox` re-state the DR-16
		// gate over the set (it FAILS CLOSED on null as of TASK-2431, so an
		// event with an unresolved MIME mounts a viewer that renders no image),
		// and `invoker` is where the viewer aims focus on close.
		expect(emitted).toEqual([
			{
				attachmentId: 'uuid-1',
				workspaceSlug: 'ws',
				itemId: 'item-A',
				hostToken: 'apanel-1',
				images: [
					{
						id: 'uuid-1',
						alt: 'A diagram',
						filename: null,
						mime_type: 'image/png',
						size_bytes: 4096,
						width: null,
						height: null,
					},
				],
				index: 0,
				invoker: img,
				filename: null,
				mime_type: 'image/png',
				size_bytes: 4096,
			},
		]);
	});

	it('emits nothing at all when the MIME cannot be resolved', async () => {
		// The revised rule for this task (TASK-2431's adversarial round): a
		// `transient` probe is NOT "unknown, so keep today's behaviour" any more.
		// `Lightbox` fails closed on an unresolved MIME, so emitting one would be
		// a request that opens nothing while looking, on the bus, exactly like a
		// request that does. The retryable branch is TASK-2434's.
		probeMock.mockResolvedValue({ status: 'transient' });
		editor = makeEditor(target);
		const img = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!img) throw new Error('image NodeView did not render');

		img.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		await settle();

		expect(emitted).toEqual([]);
	});

	it('inertizes the transform toolbar when the attachment is deleted', async () => {
		// A confirmed deletion inertizes the WHOLE node. Rotate and crop against
		// a row that is gone can only 404, and leaving them live is the same
		// dead-control gap the placeholder's role/tabindex removal closes.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png' , size: 4096 });
		editor = makeEditor(target);
		editor.commands.setNodeSelection(1);
		await Promise.resolve();
		await Promise.resolve();

		const buttons = () =>
			Array.from(
				target.querySelectorAll<HTMLButtonElement>('.attachment-image-toolbar-btn')
			);
		expect(buttons().length).toBeGreaterThan(0);

		for (const fn of deletionListeners) fn('uuid-1');

		expect(buttons().every((b) => b.disabled)).toBe(true);
		expect(buttons()[0].title).toBe('This attachment has been deleted');
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
