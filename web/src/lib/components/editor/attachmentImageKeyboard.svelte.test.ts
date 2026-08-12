// The inline body image's keyboard contract (PLAN-2392 DR-12 / TASK-2432).
//
// An inline `![alt](pad-attachment:UUID)` in an item body or a comment renders
// through the AttachmentImage NodeView, and until now that `<img>` opened the
// viewer on click and was unreachable by any other means: no role, no tabindex,
// no name. This spec pins the button contract that closes that, and — more
// importantly — pins the two ways the contract is easy to get WRONG:
//
//   - activation firing TWICE. A duplicate is easy to hide visually, so every
//     activation assertion here is a COUNT of open-viewer REQUESTS on the bus,
//     never a truthiness check. `toBe(1)` fails on 2; `not.toBeNull()` does not.
//     Counting requests rather than DOM is also what survived TASK-2433: the
//     NodeView no longer opens anything itself, so a spec that counted overlays
//     would now assert 0 forever and pass against an implementation that emits
//     nothing at all.
//
//   - the KEYBOARD path bypassing the MIME gate. The gate used to live inside
//     the click handler, which made it a property of the mouse. Both routes now
//     call one `activate()`, and the refusal is asserted through the KEYBOARD,
//     which is the assertion that would have caught a second emitter.
//
// THIS FILE IS THE PRODUCER LAYER, and its counts are counts of REQUESTS on the
// bus, not of surfaces on screen: `notifyAttachmentSurfaceOpen` is mocked, so
// "opens once" here means "asks once". That is the right layer for the keyboard
// contract — which is about how many times activation fires, and under which
// gestures — but it is deliberately blind to whether anything is listening. The
// end of the route (real bus → real host → real surface in the document) is
// pinned next door in `attachmentImageViewerHost.svelte.test.ts`.
//
// ONE SURFACE CHANNEL (TASK-2489). This NodeView used to FORK on MIME: a raster
// type opened the image viewer and a non-raster one was redirected to the options
// panel, each on its own channel. Those two channels have CONVERGED — and their
// notifiers were since removed (TASK-2490) — every resolved-ok activation now
// emits exactly ONE `notifyAttachmentSurfaceOpen`, carrying the attachment's true MIME (raster png,
// non-raster svg/pdf alike). The surface's OWN renderer picks the arm downstream
// (raster → `<img>`; svg/pdf → the no-bytes fallback), so the producer no longer
// branches on MIME. `emitted` therefore holds ALL activation emits; there is no
// second array. The NODE SEMANTICS are UNCHANGED across the convergence: a known
// non-raster MIME still gets the `Attachment options: <alt>` aria-label and a
// raster one `View image: <alt>`, and role/tabindex behave identically — only the
// EMIT collapsed to one channel.
//
// Driven through a REAL Tiptap editor, like the placeholder spec next door: the
// semantics live on imperative NodeView DOM and a hand-built element would pin
// nothing about the code under test.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import { isEditorOwnedImage } from '$lib/attachments/editorOwnedImage';

const deletionListeners = new Set<(uuid: string) => void>();
// Open-surface requests, captured RAW — before the channel's addressability
// filter, so what is asserted is what THIS NodeView produced. TASK-2489
// converged the old viewer/panel fork into ONE channel, so this array now holds
// EVERY activation emit (raster png, non-raster svg/pdf alike); there is no
// second array to tell a redirect from a drop, because there is no redirect.
const emitted: Array<Record<string, unknown>> = [];
// The restore channel FANS OUT like the deletion one above: a mock that only
// records the subscription leaves the NodeView subscribed to nothing, and every
// test of how it REACTS passes vacuously (BUG-2509).
const restoreListeners = new Set<(event: { workspaceSlug: string; itemId: string }) => void>();

vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentSurfaceOpen: (event: Record<string, unknown>) => {
		emitted.push(event);
	},
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
	registerAttachmentParentRestoredListener: (
		fn: (event: { workspaceSlug: string; itemId: string }) => void
	) => {
		restoreListeners.add(fn);
		return () => restoreListeners.delete(fn);
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

// Args are passed through so a test can answer PER ATTACHMENT — the uuid-swap
// case below needs one image's probe to still be in flight while another's
// answers.
const probeMock = vi.fn<(ws?: string, uuid?: string) => Promise<ProbeResult>>(async () => ({
	status: 'transient',
}));
// The load-failure path's REVALIDATION, separable from the cached read above.
// It delegates to `probeMock` by default, so for almost every test the two are
// one mock and answer alike. Exactly one test needs them to differ: proving
// that a 404 reaching the ACTIVATION branch upgrades an already-hidden
// placeholder requires the error path's own probe not to be the thing that
// latches it, or the assertion could not tell the two routes apart.
const revalidateMock = vi.fn<(ws?: string, uuid?: string) => Promise<ProbeResult>>();
vi.mock('./attachment-metadata', () => ({
	fetchAttachmentMetadata: (ws: string, uuid: string) => probeMock(ws, uuid),
	revalidateAttachmentMetadata: (ws: string, uuid: string) => revalidateMock(ws, uuid),
	invalidateAttachmentMetadata: () => {},
	mimeToFormat: () => null,
}));

const { AttachmentImage } = await import('./attachment-image');

const BODY_CONTENT = '<p><img data-attachment-id="uuid-1" src="/api/v1/x" alt="A diagram"></p>';

/**
 * The address every editor below reads through. MUTABLE, because the real
 * reader is: `CommentEditor` is deliberately reused across an item switch and
 * its address changes under a mounted NodeView (see hostAddress.ts). Activation
 * now spans an await, so that switch can land mid-flight.
 */
let address = { workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' };

/** The item-body configuration (Editor.svelte): transforms enabled. */
function makeEditor(element: HTMLElement, content: string = BODY_CONTENT): Editor {
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
		content,
		editable: true,
	});
}

/**
 * The COMMENT configuration, copied from CommentEditor.svelte: it configures
 * AttachmentImage independently, with transforms deliberately switched off.
 * Asserted separately because "the other surface configures it too" is exactly
 * the kind of thing that gets the behaviour only on the surface you tested.
 */
function makeCommentEditor(element: HTMLElement): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentImage.configure({
				getDownloadUrl: (uuid: string) => `/api/v1/workspaces/ws/attachments/${uuid}`,
				workspaceSlug: 'ws',
				address: () => address,
				supportedFormats: () => [] as string[],
				transform: async () => {
					throw new Error('Image transforms are not available in comments.');
				},
			}),
		],
		content: BODY_CONTENT,
		editable: true,
	});
}

/**
 * How many times activation asked for the SURFACE. One request per open.
 *
 * Since TASK-2489 there is a single channel: every resolved-ok activation emits
 * exactly one surface event regardless of MIME, so this is simply the count of
 * activation emits — no viewer/panel split to keep apart anymore.
 */
function openCount(): number {
	return emitted.length;
}

/**
 * Activation resolves the image's MIME BEFORE emitting (TASK-2433), so it is
 * asynchronous even on a cache hit. Every post-gesture count goes through here;
 * a synchronous read would be 0 regardless of what the implementation did.
 */
async function opened(): Promise<number> {
	// TWO turns of the macrotask queue, not one. A count read too early is
	// conservative for the "opens once" cases (an emission that had not
	// happened yet reads as 0 and FAILS the assertion) but not for the "opens
	// nothing" ones, where an implementation that emitted one tick later would
	// pass. The second await closes that.
	await new Promise((resolve) => setTimeout(resolve, 0));
	await new Promise((resolve) => setTimeout(resolve, 0));
	return emitted.length;
}

/**
 * A stand-in for ItemTimeline's delegated keydown handler, built from the same
 * pieces as the real one: the same `img[data-attachment-id]` selector, the same
 * activation keys, the same modifier guard, and — critically — the same
 * `isEditorOwnedImage` ownership check that keeps it off a live editor's DOM.
 *
 * It OPENS rather than just counting. A counter alone proves the event escaped
 * but leaves openCount() at 1, so the failure that actually matters — two
 * viewers on screen from one keypress — would not show up in the assertion.
 */
function timelineDelegation(
	onFire: () => void,
	opts: { ownership: boolean } = { ownership: true }
): (e: Event) => void {
	return (e: Event) => {
		const ev = e as KeyboardEvent;
		if (ev.key !== 'Enter' && ev.key !== ' ') return;
		if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.shiftKey) return;
		const img = (e.target as HTMLElement | null)?.closest<HTMLElement>(
			'img[data-attachment-id]'
		);
		if (!img) return;
		// `ownership: false` is the PRE-TASK-2432 shape, kept deliberately: the
		// node's own stopPropagation has to hold up against a delegated opener
		// that does NOT know to skip editor-owned DOM, because ItemTimeline is
		// not the only surface that could ever delegate over one.
		if (opts.ownership && isEditorOwnedImage(img)) return;
		onFire();
		// A stand-in for the timeline's OWN viewer, deliberately a different
		// element from anything the NodeView produces: these tests are about
		// two viewers from one gesture, which is only observable if the two
		// are distinguishable.
		const stub = document.createElement('div');
		stub.className = 'timeline-viewer-stub';
		document.body.appendChild(stub);
	};
}

describe('inline body image — keyboard activation (DR-12)', () => {
	let host: HTMLElement;
	let target: HTMLElement;
	let editor: Editor | undefined;

	beforeEach(() => {
		deletionListeners.clear();
		address = { workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' };
		probeMock.mockClear();
		// The ORDINARY case is an image whose MIME resolves to an allowlisted
		// raster type, and as of TASK-2433 that is a PRECONDITION of opening at
		// all: activation resolves the MIME first and emits only on a positive
		// answer, because `Lightbox` fails closed on an unresolved one. A
		// `transient` default would make every "opens exactly once" test below
		// assert 0 for a reason that has nothing to do with the keyboard.
		probeMock.mockResolvedValue({ status: 'ok' as const, mime: 'image/png', size: 4096 });
		revalidateMock.mockClear();
		revalidateMock.mockImplementation((ws?: string, uuid?: string) => probeMock(ws, uuid));
		emitted.length = 0;
		host = document.body.appendChild(document.createElement('div'));
		target = host.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		editor?.destroy();
		editor = undefined;
		host.remove();
		emitted.length = 0;
		document.querySelectorAll('.timeline-viewer-stub').forEach((d) => d.remove());
	});

	function image(): HTMLImageElement {
		const el = target.querySelector<HTMLImageElement>('img[data-attachment-id]');
		if (!el) throw new Error('image NodeView did not render');
		return el;
	}

	function press(el: HTMLElement, key: string, mods: KeyboardEventInit = {}): KeyboardEvent {
		const ev = new KeyboardEvent('keydown', {
			key,
			bubbles: true,
			cancelable: true,
			...mods,
		});
		el.dispatchEvent(ev);
		return ev;
	}

	function click(el: HTMLElement): MouseEvent {
		const ev = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
		el.dispatchEvent(ev);
		return ev;
	}

	/** Let the lazy HEAD probe (armed by selecting the node) settle. */
	async function settleProbe() {
		editor?.commands.setNodeSelection(1);
		await Promise.resolve();
		await Promise.resolve();
		await Promise.resolve();
	}

	it('is a named, focusable button', () => {
		editor = makeEditor(target);
		const img = image();

		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');
		// Name from alt. There is no filename on the node's attrs and the HEAD
		// metadata carries none either, so the filename form DR-12 sketches has
		// no source here — alt, then a generic fallback.
		expect(img.getAttribute('aria-label')).toBe('View image: A diagram');
	});

	it('falls back to a generic name when the image has no alt', () => {
		editor = makeEditor(target, '<p><img data-attachment-id="uuid-1" src="/api/v1/x"></p>');
		expect(image().getAttribute('aria-label')).toBe('View attachment image');
	});

	it('opens on Enter, exactly once', async () => {
		editor = makeEditor(target);
		expect(openCount()).toBe(0);

		press(image(), 'Enter');

		// A COUNT, not a truthiness check: a second emitter (a keyboard path that
		// activated on its own AND a synthesized click) shows up here as 2 and is
		// invisible to `not.toBeNull()`.
		expect(await opened()).toBe(1);
	});

	it('emits the viewer request the deleted dialog used to open', async () => {
		// TASK-2433 deleted this NodeView's hand-rolled `<dialog>`. Every other
		// test in this file counts activations, and a count is satisfied by an
		// emission of any shape — so exactly one test has to pin the CONTENT, or
		// "the dialog is gone" would be provable by an implementation that
		// replaced it with nothing.
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		await opened();

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
				// The focus-restore target. The <img> is a real focus stop as of
				// TASK-2432, so the keyboard path has a stable one to offer —
				// which is the whole reason `invoker` is on the event.
				invoker: img,
				// Single-open seeds describing images[0] (TASK-2489). Null filename,
				// same absence the node's attrs and the HEAD both carry.
				filename: null,
				mime_type: 'image/png',
				size_bytes: 4096,
			},
		]);
	});

	it('opens on Space exactly once and suppresses the page scroll', async () => {
		editor = makeEditor(target);
		expect(openCount()).toBe(0);

		const ev = press(image(), ' ');

		expect(await opened()).toBe(1);
		// Unhandled Space scrolls the document — and inside a contenteditable it
		// would also be taken as text input against the selected atom.
		expect(ev.defaultPrevented).toBe(true);
	});

	it('accepts the legacy "Spacebar" key name too', async () => {
		// Same alias the file chip next door accepts — older engines report Space
		// under this name, and an image that ignored it would be inert there.
		editor = makeEditor(target);
		expect(openCount()).toBe(0);

		const ev = press(image(), 'Spacebar');

		expect(await opened()).toBe(1);
		expect(ev.defaultPrevented).toBe(true);
	});

	it('opens on a mouse click, exactly once', async () => {
		editor = makeEditor(target);
		expect(openCount()).toBe(0);
		click(image());
		expect(await opened()).toBe(1);
	});

	// Both activation keys, because the propagation stop is per-key: a handler
	// that stopped for Enter and not Space would pass an Enter-only test and
	// still double-open on the key most people press.
	for (const key of ['Enter', ' ']) {
		it(`does not double-open when ${key === ' ' ? 'Space' : key} lands inside a delegated container`, async () => {
			// ItemTimeline delegates thumbnail click/keydown across its whole entry
			// list, and that list CONTAINS live CommentEditor instances whose bodies
			// render this NodeView — an `img[data-attachment-id]`, which is exactly
			// the selector the delegation matches. Without stopPropagation, one
			// keypress activates the node AND the timeline's own opener: two viewers.
			//
			// The stand-in ancestor OPENS, rather than just counting: a counter
			// alone proves the event escaped but leaves the count at 1, so the
			// failure it is actually about — two viewers on screen — would not be
			// visible in the assertion that matters.
			let delegated = 0;
			host.addEventListener(
				'keydown',
				timelineDelegation(() => (delegated += 1), { ownership: false })
			);
			editor = makeCommentEditor(target);
			expect(openCount()).toBe(0);

			press(image(), key);

			expect(delegated).toBe(0);
			expect(await opened()).toBe(1);
		});
	}

	// A modified key is a shortcut, not an activation. Cmd/Ctrl+Enter is
	// CommentEditor's SUBMIT binding and an image is a perfectly ordinary thing
	// to have focused when you post — an activation handler that matched on
	// `key` alone would swallow it and open a viewer instead of posting.
	for (const [label, mods] of [
		['Ctrl+Enter', { ctrlKey: true }],
		['Meta+Enter', { metaKey: true }],
		['Shift+Enter', { shiftKey: true }],
		['Alt+Space', { altKey: true }],
	] as const) {
		it(`treats ${label} as a shortcut, not an activation`, async () => {
			let seen = 0;
			let timelineOpened = 0;
			host.addEventListener('keydown', () => (seen += 1));
			// The REAL ItemTimeline delegation, ownership check included. A
			// modified key is deliberately let through so CommentEditor's submit
			// binding can see it — which means it also reaches this handler, and
			// that handler must not turn a submit into a viewer.
			host.addEventListener(
				'keydown',
				timelineDelegation(() => (timelineOpened += 1))
			);
			editor = makeCommentEditor(target);
			const key = label.endsWith('Space') ? ' ' : 'Enter';

			// Isolated first — NOT bubbling, so nothing but this node's own
			// listener can touch the event. ProseMirror's keymap legitimately
			// consumes some of these combinations once they reach the editor
			// root, and asserting on the bubbled event would be measuring that
			// instead of the handler under test.
			const solo = new KeyboardEvent('keydown', { key, cancelable: true, ...mods });
			image().dispatchEvent(solo);
			expect(await opened()).toBe(0);
			expect(solo.defaultPrevented).toBe(false);

			// Then bubbling, for the other half: the event must still REACH the
			// surface that owns the shortcut. A handler that declined to
			// activate but kept swallowing the event would break the binding
			// just as thoroughly as one that opened a viewer.
			press(image(), key, mods);
			expect(seen).toBe(1);
			expect(await opened()).toBe(0);
			expect(timelineOpened).toBe(0);
		});
	}

	it('opens once for a HELD key, not once per repeat', async () => {
		// Every repeat is another keydown. Without a guard, leaning on Enter
		// stacks a viewer per repeat — "exactly once" has to mean once per
		// gesture. A truthiness check would not see this at all.
		let delegated = 0;
		host.addEventListener(
			'keydown',
			timelineDelegation(() => (delegated += 1), { ownership: false })
		);
		editor = makeEditor(target);

		const img = image();
		press(img, 'Enter');
		for (let i = 0; i < 4; i++) press(img, 'Enter', { repeat: true });

		expect(await opened()).toBe(1);
		// And the repeats stay suppressed: they belong to the activation the
		// user made once, so letting them escape would hand the surrounding
		// surface four keypresses that never happened.
		expect(delegated).toBe(0);
	});

	it('still reaches the delegated container for keys it does not handle', async () => {
		// The propagation stop is scoped to the activation keys; swallowing
		// everything would break Escape, Tab-adjacent handlers and the editor's
		// own key handling on the surrounding surface.
		let seen = 0;
		host.addEventListener('keydown', () => (seen += 1));
		editor = makeEditor(target);

		const ev = press(image(), 'Escape');

		expect(seen).toBe(1);
		// Nor is it swallowed: an Escape the node consumed would never reach the
		// surface that closes a pane or a dialog.
		expect(ev.defaultPrevented).toBe(false);
		expect(await opened()).toBe(0);
	});

	it('emits ONE surface event carrying the svg MIME through the KEYBOARD, not just the mouse', async () => {
		// Convergence (TASK-2489): the producer no longer keeps svg off the raster
		// arm — the SURFACE does, downstream, by picking its no-bytes fallback for a
		// non-raster MIME. So the keyboard path emits exactly ONE surface event
		// carrying the true `image/svg+xml`, and it is that MIME — not a second
		// channel — that keeps the bytes off the <img> arm. Asserting the emit (not
		// merely "no viewer") preserves the intent: the gesture is not dropped.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml' , size: 4096 });
		editor = makeEditor(target);

		// BEFORE the probe answers, this still LOOKS like an ordinary button:
		// the semantics pass keeps role/tabindex while the MIME is merely
		// unasked, because "not yet asked" is not "not viewable". (Activation
		// itself no longer trusts that — TASK-2433 made it resolve the MIME
		// first — but the two are separate claims and this one is the premise.)
		// Pinning it here is what makes the emit below attributable to the RESOLVED
		// MIME rather than to any other route — an empty uuid, a latched deletion,
		// a hidden image — all of which would already be true now.
		expect(image().getAttribute('role')).toBe('button');

		await settleProbe();
		expect(probeMock).toHaveBeenCalled();

		press(image(), 'Enter');
		expect(await opened()).toBe(1);
		// The surface event carries the svg MIME — flat seed and images[0] alike.
		expect(emitted[0].mime_type).toBe('image/svg+xml');
		expect((emitted[0].images as Array<Record<string, unknown>>)[0].mime_type).toBe(
			'image/svg+xml'
		);
		// The node SEMANTICS are unchanged by the convergence: a known non-raster
		// MIME still names itself the options control and stays a real focus stop.
		expect(image().getAttribute('role')).toBe('button');
		expect(image().getAttribute('tabindex')).toBe('0');
		expect(image().getAttribute('aria-label')).toBe('Attachment options: A diagram');
	});

	it('emits a surface event for a non-allowlisted MIME, whole payload', async () => {
		// The `ok` + not-allowlisted arm of the matrix, asserted on what is
		// EMITTED. A count alone is satisfied by an event of any shape, and the
		// surface's routing fields are exactly what a producer gets wrong: the
		// address decides which of two mounted hosts opens it, and the metadata
		// fields are what the surface renders before its own fetch lands. Since
		// TASK-2489 a pdf takes the ONE converged channel — the same surface event
		// a raster type gets, carrying the true `application/pdf`.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 1234 });
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		expect(await opened()).toBe(1);

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
						// No filename anywhere on this surface — the node's attrs carry
						// none and the HEAD does not either. Null, not a fabricated one.
						filename: null,
						mime_type: 'application/pdf',
						size_bytes: 1234,
						width: null,
						height: null,
					},
				],
				index: 0,
				invoker: img,
				filename: null,
				mime_type: 'application/pdf',
				size_bytes: 1234,
			},
		]);
		// It ASKED. A payload alone is satisfiable by an implementation that
		// skipped the probe and assumed a MIME — which is precisely the gate
		// this whole path exists to close.
		expect(probeMock).toHaveBeenCalled();
		// And the pending affordance is cleared on THIS branch too. The
		// finalizer is shared, but a clear moved into a MIME-specific branch would
		// leave a non-raster emit permanently `aria-busy`.
		expect(img.getAttribute('aria-busy')).toBeNull();
		expect(img.style.cursor).toBe('');
	});

	it('keeps emitting a surface event on every activation, not just the first', async () => {
		// The regression the semantics rewrite exists to prevent, and the one an
		// attribute-only assertion would miss entirely. Activation now WRITES the
		// resolved MIME onto the node. If the activation gate refused a known
		// non-allowlisted MIME — as it did before the convergence — the first tap
		// would emit and every tap after it would silently do nothing, because the
		// gate would be reading the answer the first tap recorded.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 10 });
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		await opened();
		expect(emitted).toHaveLength(1);

		// The premise: the MIME really is on the node now (the label proves it,
		// and it is the same state the gate would have been reading).
		expect(img.getAttribute('aria-label')).toBe('Attachment options: A diagram');

		press(img, 'Enter');
		await opened();
		click(img);
		await opened();

		expect(emitted).toHaveLength(3);
		// The LAST one, not just the count: an emit that kept firing with a payload
		// frozen at the first gesture would be a count of three and a surface that
		// opens on whatever the node used to be.
		expect(emitted[2]).toMatchObject({
			attachmentId: 'uuid-1',
			itemId: 'item-A',
			hostToken: 'apanel-1',
			invoker: img,
			mime_type: 'image/svg+xml',
			size_bytes: 10,
		});
	});

	it('emits exactly one surface event, never two', async () => {
		// There is ONE channel now (TASK-2489), so a raster type has exactly one
		// place to go — and it must go there exactly once. An implementation that
		// still emitted on two paths (a leftover viewer emit AND a converged one)
		// would open two surfaces from one gesture; a count of one closes that.
		editor = makeEditor(target);

		press(image(), 'Enter');

		expect(await opened()).toBe(1);
		expect(emitted).toHaveLength(1);
		// Reached by ASKING, not by assuming: an implementation that skipped the
		// probe for images it liked the look of would pass the assertion above
		// and be exactly the bypass TASK-2433 closed.
		expect(probeMock).toHaveBeenCalled();
	});

	it('latches the permanent placeholder on an authoritative 404, and opens nothing', async () => {
		// The `missing` arm. `missing` is the ONLY result that may latch (DR-17),
		// and the latch is what keeps editor undo from resurrecting a deleted
		// attachment as a live-looking node.
		probeMock.mockResolvedValue({ status: 'missing' });
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		await opened();

		expect(await opened()).toBe(0);
		expect(emitted).toEqual([]);

		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		expect(img.style.display).toBe('none');
		expect(placeholder?.style.display).not.toBe('none');
		// PERMANENT, and the copy says so: a deleted row is not retryable, so the
		// placeholder is deliberately NOT a control. A retryable placeholder here
		// would invite a click that can only 404.
		expect(placeholder?.title).toBe('This attachment has been deleted');
		expect(placeholder?.getAttribute('role')).toBeNull();
		expect(placeholder?.getAttribute('tabindex')).toBeNull();
		// And the latch holds against BOTH ways back. A click cannot retry it...
		placeholder?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(img.style.display).toBe('none');
		// ...and neither can a `load` that was already in flight when the 404
		// landed. The implementation stops that twice over — the latch detaches
		// the listener AND `resetMissing` refuses to run once `deleted` — so this
		// pins the OUTCOME rather than either mechanism, which is the honest
		// claim to make about a guard with a redundant partner.
		img.dispatchEvent(new Event('load'));
		expect(img.style.display).toBe('none');
		expect(img.getAttribute('aria-busy')).toBeNull();
	});

	it('leaves a transient failure RETRYABLE — no open, no latch', async () => {
		// The `transient` arm, and the dead focus stop TASK-2433 left behind: it
		// emitted nothing and did nothing, so a focused image announced itself as
		// a button and silently swallowed every press.
		probeMock.mockResolvedValue({ status: 'transient' });
		editor = makeEditor(target);
		const img = image();
		img.focus();

		press(img, 'Enter');
		await opened();

		expect(await opened()).toBe(0);
		expect(emitted).toEqual([]);

		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		expect(img.style.display).toBe('none');
		expect(placeholder?.style.display).not.toBe('none');
		// RETRYABLE, not latched — the copy, the semantics and the focus all say
		// the same thing. This is the assertion that separates it from `missing`:
		// an implementation that treated the two alike would pass every "did not
		// open" check above and permanently strand a row that is perfectly fine.
		expect(placeholder?.title).toContain('Click to retry');
		expect(placeholder?.getAttribute('role')).toBe('button');
		expect(placeholder?.getAttribute('tabindex')).toBe('0');
		// The keypress that reported the failure must not drop focus to <body>.
		expect(document.activeElement).toBe(placeholder);

		// And the latch really is absent: Retry restores the image, which
		// `missing` above cannot do.
		expect(img.getAttribute('aria-busy')).toBeNull();
		const srcBefore = img.getAttribute('src');
		placeholder?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(img.style.display).toBe('');
		expect(placeholder?.style.display).toBe('none');
		// Retry means a NEW REQUEST, not just an un-hidden element: without the
		// cache-busting query the browser replays the failed entry and the retry
		// is theatre. Restoring the DOM alone would pass the two lines above.
		expect(img.getAttribute('src')).not.toBe(srcBefore);
		expect(img.getAttribute('src')).toContain('retry=');
	});

	it('does not let a transient failure latch permanently across a recovery', async () => {
		// DR-17's rule stated over TIME rather than over one result: only an
		// authoritative 404 latches, so a blip followed by a healthy probe must
		// leave the image fully openable again. An implementation that reused the
		// `missing` path for `transient` would fail here and NOWHERE else — every
		// single-shot assertion above would still pass.
		probeMock.mockResolvedValue({ status: 'transient' });
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		await opened();
		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		// The premise, and it is load-bearing: without it this passes against an
		// implementation whose `transient` arm does nothing at all — there would
		// be no latch to survive, and the recovery below would prove nothing.
		expect(img.style.display).toBe('none');
		expect(placeholder?.getAttribute('role')).toBe('button');
		placeholder?.dispatchEvent(new MouseEvent('click', { bubbles: true }));

		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 4096 });
		press(img, 'Enter');

		expect(await opened()).toBe(1);
	});

	it('shows a pending affordance while the MIME is still resolving', async () => {
		// A cold probe is a round trip, and a click that does nothing for its
		// duration reads as broken. Deliberately minimal: `aria-busy` and a
		// cursor, no new chrome.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
				})
		);
		editor = makeEditor(target);
		const img = image();
		expect(img.getAttribute('aria-busy')).toBeNull();

		press(img, 'Enter');

		// Set SYNCHRONOUSLY with the gesture — a pending state that waits for a
		// tick is not covering the wait it exists for.
		expect(img.getAttribute('aria-busy')).toBe('true');
		expect(img.style.cursor).toBe('progress');

		release();
		expect(await opened()).toBe(1);
		// And cleared, or the image announces itself as permanently busy.
		expect(img.getAttribute('aria-busy')).toBeNull();
		expect(img.style.cursor).toBe('');
	});

	it('clears the pending affordance when a swap supersedes the request', async () => {
		// The finalizer only runs for the request that still holds the latch, so
		// a swap has to clear the pending state itself — otherwise a HEAD that
		// never settles leaves the NEW image permanently `aria-busy`.
		probeMock.mockImplementation(() => new Promise<never>(() => {}));
		editor = makeEditor(target);
		press(image(), 'Enter');
		expect(image().getAttribute('aria-busy')).toBe('true');

		// The SAME element across the swap: this NodeView deliberately survives a
		// uuid change, and reacquiring by selector would let a destroy/recreate
		// implementation pass without ever clearing anything.
		const before = image();
		editor.commands.setNodeSelection(1);
		editor.commands.updateAttributes('attachmentImage', { uuid: 'uuid-2' });

		expect(image()).toBe(before);
		expect(before.getAttribute('aria-busy')).toBeNull();
		expect(before.style.cursor).toBe('');
	});

	it('opens NOTHING when a delete lands mid-probe and the answer comes back ok', async () => {
		// The race the uuid comparison structurally cannot catch: a delete does
		// not change which attachment the node points at, so `forUuid` is still
		// current when the probe resolves — with a perfectly valid `ok` describing
		// a row the server has since dropped.
		//
		// The implementation states this TWICE — an explicit `deleted` re-check
		// and `canActivate()`'s hidden-placeholder clause — and mutation testing
		// confirms this spec fails only when BOTH are gone. That is the honest
		// claim: it pins the BEHAVIOUR, not either line. (The two are equivalent
		// today only because every path that sets `deleted` also hides the image;
		// nothing enforces that, which is why the check is stated on its own
		// terms rather than inferred from the placeholder.)
		//
		// A test that let the probe resolve BEFORE the delete would pass against
		// an implementation with no `deleted` check at all, so the ordering here
		// is the whole point: the answer is held until after the broadcast.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
				})
		);
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		for (const fn of deletionListeners) fn('uuid-1');
		// The premise: the node still points at the very attachment the probe is
		// about. Without this the drop could be the uuid fence doing the work.
		expect(img.getAttribute('data-attachment-id')).toBe('uuid-1');
		release();

		expect(await opened()).toBe(0);
		// ONE channel now (TASK-2489). A `deleted` check placed after the old
		// allowlist branch would still have leaked; the converged emit means the
		// single array must stay empty.
		expect(emitted).toEqual([]);
	});

	it('emits NOTHING either when a delete lands mid-probe on a non-raster type', async () => {
		// The same race down the converged branch for a non-raster type: the
		// `missing`/`transient`/`ok` split gave the continuation three more places
		// to act on a row that is gone, and a non-raster MIME no longer takes a
		// separate path, so the surface emit must be suppressed just the same.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/svg+xml', size: 10 });
				})
		);
		editor = makeEditor(target);

		press(image(), 'Enter');
		for (const fn of deletionListeners) fn('uuid-1');
		release();

		await opened();
		expect(emitted).toEqual([]);
		expect(await opened()).toBe(0);

		// THE CONTROL, and it is the whole test: `expect no emit` is satisfied by
		// an implementation that never emits for a non-raster type at all — the
		// pre-convergence binary refusal passes it outright. So prove the branch
		// works on an undeleted node under the identical probe: it emits once.
		editor.destroy();
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 10 });
		editor = makeEditor(target);
		press(image(), 'Enter');
		await opened();
		expect(emitted).toHaveLength(1);
	});

	// The address fence, restated for a non-raster type. It is the same emission
	// site, now the ONLY one, so the three comparisons have to hold for it — a
	// fence that only guarded a raster emit would let a non-raster one open a
	// surface over a pane the user has left.
	for (const [label, moved] of [
		['workspace', { workspaceSlug: 'ws2', itemId: 'item-A', hostToken: 'apanel-1' }],
		['item', { workspaceSlug: 'ws', itemId: 'item-B', hostToken: 'apanel-1' }],
		['owning mount', { workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-2' }],
	] as const) {
		it(`drops the surface emit for a non-raster type when the ${label} moves mid-resolution`, async () => {
			probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml', size: 10 });
			editor = makeCommentEditor(target);

			press(image(), 'Enter');
			address = { ...moved };

			await opened();
			expect(emitted).toEqual([]);

			// The control: settled, the next gesture DOES emit — so the drop above
			// is the fence, not a branch that never worked.
			press(image(), 'Enter');
			await opened();
			expect(emitted).toHaveLength(1);
			expect(emitted[0].itemId).toBe(moved.itemId);
			expect(emitted[0].hostToken).toBe(moved.hostToken);
		});
	}

	it('emits with the address CAPTURED at the gesture, even if the reader mutates in place', async () => {
		// The fence holds three PRIMITIVES, not the object the reader returned.
		// Both live readers build a fresh object literal per call, so holding the
		// reference would be safe — for that reason alone, which no interface
		// states. This pins the independence: a reader that hands back ONE object
		// and mutates it in place would, if the fence held the reference, rewrite
		// the snapshot it compares against and pass unconditionally.
		const shared = { workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-1' };
		address = shared;
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
				})
		);
		editor = makeCommentEditor(target);

		press(image(), 'Enter');
		// The host "moves" by mutating the object the reader keeps handing out.
		shared.itemId = 'item-B';
		shared.hostToken = 'apanel-2';
		release();

		// Dropped. Holding the reference would compare item-B against item-B.
		expect(await opened()).toBe(0);
	});

	it('DOES emit when the address leaves and returns (A→B→A) — documented, accepted', async () => {
		// The fence compares VALUES, so an address that round-trips reads as
		// unchanged. Reachable: the pane's `ItemDetail` has no `{#key}`
		// (PLAN-2105 / TASK-2112), so an A→B→A item switch keeps one host token
		// and the composer it owns is reused across it.
		//
		// PINNED AS THE CURRENT BEHAVIOUR, not asserted as ideal. It is accepted
		// because the outcome differs from what the fence prevents — the user is
		// back on the pane, and the gesture is not re-attributed: the same node,
		// the same attachment, the same host. Contrast the uuid A→B→A case below,
		// which IS dropped, because there the SUBJECT of the gesture changed.
		//
		// Telling A→B→A from A needs an epoch on `AttachmentHostAddress` that
		// every host bumps; this NodeView only READS the address, so a B between
		// the two reads is invisible to it by construction. When that epoch
		// lands, this expectation flips to 0 and this comment is its changelog.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
				})
		);
		editor = makeCommentEditor(target);
		const original = { ...address };

		press(image(), 'Enter');
		address = { workspaceSlug: 'ws', itemId: 'item-B', hostToken: 'apanel-1' };
		address = { ...original };
		release();

		expect(await opened()).toBe(1);
		// And it emits at A — not at the B it passed through.
		expect(emitted[0].itemId).toBe(original.itemId);
		expect(emitted[0].hostToken).toBe(original.hostToken);
	});

	it('UPGRADES an already-hidden retryable placeholder when a 404 arrives', async () => {
		// The transient→missing transition, and the only DR-17 boundary that had
		// no test. The mutant it exists for is exact: an implementation that
		// processed `missing` only when `canActivate()` is true would pass every
		// other test in this file, because the placeholder-showing state is
		// precisely where `canActivate()` is already false. That is why the
		// `missing` branch deliberately runs BEFORE the presentability check — a
		// 404 is authoritative whatever the node happens to be showing.
		//
		// Reaching that state takes care. An activation cannot START while the
		// placeholder is up (the gesture-time gate refuses), and Retry UN-hides
		// the image — so a naive "fail, retry, 404" sequence lands the 404 on a
		// VISIBLE image and tests nothing new. The image has to go behind the
		// placeholder DURING the await, which is what the load `error` does.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'missing' });
				})
		);
		// The error path runs its OWN revalidation, which latches on a 404 too.
		// Keeping it transient is what makes the latch below attributable to the
		// activation branch rather than to the load failure's probe.
		revalidateMock.mockResolvedValue({ status: 'transient' });
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		img.dispatchEvent(new Event('error'));
		await opened();

		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		// The premise, and the whole point: hidden, and still RETRYABLE. Without
		// both, the 404 below would be landing on a state some other test covers.
		expect(img.style.display).toBe('none');
		expect(placeholder?.getAttribute('role')).toBe('button');
		expect(placeholder?.title).toContain('Click to retry');

		// The 404 lands while the placeholder is up.
		release();
		await opened();

		// Upgraded: permanent, inert, no longer inviting a retry that can only
		// 404 again.
		expect(placeholder?.title).toBe('This attachment has been deleted');
		expect(placeholder?.getAttribute('role')).toBeNull();
		expect(placeholder?.getAttribute('tabindex')).toBeNull();
		// And the latch holds — a retry click cannot undo an authoritative 404.
		placeholder?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(img.style.display).toBe('none');
		expect(await opened()).toBe(0);
		expect(emitted).toEqual([]);
	});

	it('opens nothing when a load failure hides the image mid-probe and the MIME is FINE', async () => {
		// The post-await `canActivate()` guard, which nothing else reaches. The
		// spec below ("will not ACTIVATE while the image is showing a
		// load-failure placeholder") fails the load FIRST and gestures after, so
		// it is stopped by the GESTURE-time gate and would pass with the
		// post-await check deleted entirely.
		//
		// This is the other order, and it is the reachable one: the gesture lands
		// on a healthy image, the load fails while the HEAD is in flight, and the
		// HEAD comes back with a perfectly good `image/png`. Nothing is deleted
		// and nothing is missing — the only reason not to open is that there is
		// no longer an image on screen to open. Emitting here would put a viewer
		// over a placeholder.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
				})
		);
		// The error path's own revalidation stays transient, so the placeholder
		// remains the RETRYABLE one — this test is about a healthy MIME meeting a
		// hidden image, not about a latch.
		revalidateMock.mockResolvedValue({ status: 'transient' });
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		img.dispatchEvent(new Event('error'));
		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		// The premise: hidden, retryable, NOT deleted — so every other guard in
		// the continuation (uuid, generation, `deleted`) is satisfied and only
		// presentability is left to do the work.
		expect(img.style.display).toBe('none');
		expect(placeholder?.getAttribute('role')).toBe('button');

		release();
		await opened();

		expect(await opened()).toBe(0);
		expect(emitted).toEqual([]);
		// And it stayed retryable: refusing to open must not cost the user the
		// retry affordance.
		expect(placeholder?.getAttribute('role')).toBe('button');
	});

	it('emits SYNCHRONOUSLY with the fence check, not on a later turn', async () => {
		// The fence's correctness rests on the check and the emit being
		// adjacent: anything queued between them — a timer, a microtask hop — is
		// a new window for the address to go stale across, which is the whole
		// hazard the fence exists for. Nothing else in this file would notice a
		// refactor that queued delivery, because every other assertion goes
		// through `opened()`, which waits two MACROtask turns and would happily
		// observe a `setTimeout(0)` emission.
		//
		// So: resolve the probe and read the count after a bounded number of
		// MICROtasks. A delivery queued behind a timer cannot be observed here;
		// a synchronous one always is.
		let release: () => void = () => {};
		probeMock.mockImplementation(
			() =>
				new Promise((resolve) => {
					release = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
				})
		);
		editor = makeEditor(target);

		press(image(), 'Enter');
		release();

		// Three microtasks is generous for the implementation's own `.then`
		// chain and still strictly inside the current macrotask.
		await Promise.resolve();
		await Promise.resolve();
		await Promise.resolve();
		expect(emitted).toHaveLength(1);
	});

	// NOTE, deliberately not a test: "stamps the CAPTURED address rather than a
	// re-read one" is UNOBSERVABLE from outside this function, and writing a test
	// that appears to prove it would be worse than leaving the gap named. The
	// fence refuses to emit whenever the two differ, so every emission happens in
	// a state where captured and re-read are equal — a spec that moved the address
	// away and back would assert the same values under either implementation. The
	// enforceable half is the drop, and that is asserted per field above.

	it('emits for an UNPROBED non-raster type — the gesture that beats the lazy probe', async () => {
		// The bypass this task's revised gate closes, and the one the test above
		// cannot see: `settleProbe()` selects the node, which builds the toolbar
		// and runs the lazy HEAD, so by then `canActivate()` already knows the
		// MIME before activation does any work of its own. An UNSELECTED body image
		// — the state every image is in until the user touches it — has
		// `knownMime === null`, and the old gate read it only when truthy: the
		// click sailed past and opened the ORIGINAL file.
		//
		// So: never selected, no toolbar, nothing probed. Activation resolves the
		// MIME itself before emitting, and since TASK-2489 that resolution feeds
		// ONE converged channel — so an unprobed non-raster type reaches the
		// surface exactly as the probed one does, twice for two gestures, never
		// dropped and never opening the original bytes.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/svg+xml' , size: 4096 });
		editor = makeEditor(target);

		// The premise: nothing has asked yet, so the image still looks openable.
		expect(probeMock).not.toHaveBeenCalled();
		expect(image().getAttribute('role')).toBe('button');

		press(image(), 'Enter');
		expect(await opened()).toBe(1);
		click(image());
		expect(await opened()).toBe(2);
		// And it did ask — an emit reached by never probing at all would be the
		// same count for the wrong reason.
		expect(probeMock).toHaveBeenCalled();
		// Both gestures carry the resolved svg MIME on the converged channel.
		expect(emitted).toHaveLength(2);
		expect(emitted[0].mime_type).toBe('image/svg+xml');
		expect(emitted[1].mime_type).toBe('image/svg+xml');
	});

	it('emits once when two gestures land inside one resolution window', async () => {
		// Resolving the MIME before emitting introduced an await where there was
		// none, and "fires exactly once" has to survive it: two activations
		// entering that window would each clear the gate and each emit, which is
		// two viewers from one user intent. The key-repeat guard does not cover
		// this — these are two distinct, unrepeated gestures.
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		press(img, 'Enter');
		click(img);

		expect(await opened()).toBe(1);

		// And the latch RELEASES: a gesture after the window closes opens again,
		// so this is not "the second one is swallowed forever".
		press(img, 'Enter');
		expect(await opened()).toBe(2);
	});

	it('drops the request when the attachment is deleted mid-resolution', async () => {
		// The await opened a window, and everything the gate checked at gesture
		// time can stop being true inside it. A deletion is the sharpest case:
		// it is authoritative, it arrives from another surface at any moment, and
		// a request emitted after it opens a viewer on a row that is gone.
		editor = makeEditor(target);
		const img = image();

		press(img, 'Enter');
		// Same tick as the gesture, before the HEAD resolves.
		for (const fn of deletionListeners) fn('uuid-1');

		expect(await opened()).toBe(0);
	});

	it('never probes, and never emits, on a surface with no workspace', async () => {
		// An SSR / preview surface configures the extension with the unaddressed
		// reader. There is no workspace to probe under, so nothing can be
		// positively known — and the rule is "emit only on a positive answer",
		// not "emit when we could not check".
		address = { workspaceSlug: '', itemId: 'item-A', hostToken: 'apanel-1' };
		editor = makeEditor(target);

		press(image(), 'Enter');
		click(image());

		expect(await opened()).toBe(0);
		// Not merely refused after asking — never asked. A probe keyed on an
		// empty workspace is a cache entry under the wrong key.
		expect(probeMock).not.toHaveBeenCalled();
	});

	it('does not emit for a row the probe says is GONE', async () => {
		// `missing` is authoritative and distinct from `transient`: one is "the
		// row is not there", the other is "we could not tell". Neither is a
		// positively-known MIME, so neither opens — but they are separate
		// branches of the result union and a gate written as
		// `status === 'transient'` would let this one through.
		probeMock.mockResolvedValue({ status: 'missing' });
		editor = makeEditor(target);

		press(image(), 'Enter');

		expect(await opened()).toBe(0);
		// And nothing opened at all — `missing` is the one result with no
		// destination. An implementation that fell through to the converged
		// surface emit would offer a row that is gone.
		expect(emitted).toEqual([]);
	});

	it('drops the request when the NodeView is torn down mid-resolution', async () => {
		// The await outlives the editor: an item switch or a pane remount
		// destroys the view while the HEAD is in flight, and a request emitted
		// afterwards asks a host to open a viewer for a surface that is gone.
		editor = makeEditor(target);

		press(image(), 'Enter');
		editor.destroy();
		editor = undefined;

		expect(await opened()).toBe(0);
	});

	// Each field SEPARATELY, because the fence is three comparisons and a test
	// that moved only one leaves the other two free to be deleted. The workspace
	// is what every image URL is read from, `itemId` is which pane shows the
	// viewer, and `hostToken` is which of two concurrently-mounted panes it is.
	for (const [label, moved] of [
		['workspace', { workspaceSlug: 'ws2', itemId: 'item-A', hostToken: 'apanel-1' }],
		['item', { workspaceSlug: 'ws', itemId: 'item-B', hostToken: 'apanel-1' }],
		['owning mount', { workspaceSlug: 'ws', itemId: 'item-A', hostToken: 'apanel-2' }],
	] as const) {
		it(`drops the request when the ${label} moves mid-resolution`, async () => {
			// `CommentEditor` is reused across an item switch — its address
			// changes under a mounted NodeView. The gesture belonged to the OLD
			// address: emitting there opens a viewer over a pane the user has
			// left, and emitting at the new one attributes the gesture to a
			// different item.
			editor = makeCommentEditor(target);

			press(image(), 'Enter');
			address = { ...moved };

			expect(await opened()).toBe(0);

			// The control: with the address settled, the very next gesture emits
			// — so the drop above is the fence, not a composer that stopped
			// working.
			press(image(), 'Enter');
			expect(await opened()).toBe(1);
			// The probe ran under the address the GESTURE happened at — the
			// workspace keys the metadata cache, so probing under the wrong one
			// answers a question about a different workspace's row.
			expect(probeMock).toHaveBeenLastCalledWith(moved.workspaceSlug, 'uuid-1');
			expect(emitted[0].workspaceSlug).toBe(moved.workspaceSlug);
			expect(emitted[0].itemId).toBe(moved.itemId);
			expect(emitted[0].hostToken).toBe(moved.hostToken);
		});
	}

	it('does not leave the NEW image dead when a swap lands mid-resolution', async () => {
		// The one-at-a-time latch is per NodeView, and this NodeView deliberately
		// SURVIVES a uuid swap (rotate/crop, or a peer's op) rather than being
		// recreated. So a latch taken for the old attachment would still be held
		// when the new one is clicked — and since a HEAD has no timeout, an
		// activation that never settles would leave the image in front of the
		// user permanently unopenable.
		const pending = new Promise<never>(() => {});
		probeMock.mockImplementation((_ws?: string, uuid?: string) =>
			uuid === 'uuid-1'
				? (pending as unknown as Promise<{ status: 'transient' }>)
				: Promise.resolve({ status: 'ok', mime: 'image/png', size: 4096 } as never)
		);
		editor = makeEditor(target);

		press(image(), 'Enter');
		expect(await opened()).toBe(0);

		// The swap the NodeView is built to survive.
		editor.commands.setNodeSelection(1);
		editor.commands.updateAttributes('attachmentImage', { uuid: 'uuid-2' });
		await opened();

		press(image(), 'Enter');
		const requests = await opened();
		expect(requests).toBe(1);
		expect(emitted[0].attachmentId).toBe('uuid-2');
	});

	it('does not let a superseded activation unlock the one that replaced it', async () => {
		// The other half of the swap fix, and the one it is easy to get wrong:
		// the latch is released in TWO places now — the resolution's finalizer
		// and the swap — so an unconditional release lets the OLD image's HEAD,
		// landing late, unlock a request the NEW image is still holding. Two
		// gestures then both emit, which is the duplicate the latch exists to
		// prevent.
		// ONE promise per attachment, handed to every caller — the real cache
		// does exactly this (`fetchAttachmentMetadata` installs the in-flight
		// promise and shares it), and it is load-bearing here: a fresh promise
		// per call would leave the duplicate activation's continuation pending
		// forever, hiding the very duplicate this test is about.
		let releaseOld: () => void = () => {};
		let releaseNew: () => void = () => {};
		const oldProbe = new Promise((resolve) => {
			releaseOld = () => resolve({ status: 'ok', mime: 'image/png', size: 1 });
		});
		const newProbe = new Promise((resolve) => {
			releaseNew = () => resolve({ status: 'ok', mime: 'image/png', size: 4096 });
		});
		probeMock.mockImplementation(
			(_ws?: string, uuid?: string) => (uuid === 'uuid-1' ? oldProbe : newProbe) as never
		);
		editor = makeEditor(target);

		// A's activation goes in flight and is then superseded by a swap.
		press(image(), 'Enter');
		editor.commands.setNodeSelection(1);
		editor.commands.updateAttributes('attachmentImage', { uuid: 'uuid-2' });
		await opened();

		// B's activation takes the latch...
		press(image(), 'Enter');
		await opened();
		// ...and A's late answer must not hand it back.
		releaseOld();
		await opened();

		// A second gesture while B is still resolving. With the latch correctly
		// held this is a no-op; with it wrongly released, this starts a SECOND
		// resolution and both emit.
		press(image(), 'Enter');
		releaseNew();

		expect(await opened()).toBe(1);
		expect(emitted[0].attachmentId).toBe('uuid-2');
	});

	it('drops a request whose image was swapped AWAY AND BACK', async () => {
		// Comparing the uuid alone is not enough, and this is the interleaving
		// that shows it: a rotate the user immediately undoes, or a peer's op
		// reverted, puts the ORIGINAL attachment back under a request that is
		// still resolving for it. It would then find its own uuid in place and
		// open a viewer for a gesture made two swaps ago — against an image the
		// user has since acted on twice.
		let releaseFirst: () => void = () => {};
		const firstProbe = new Promise((resolve) => {
			releaseFirst = () => resolve({ status: 'ok', mime: 'image/png', size: 1 });
		});
		probeMock.mockImplementation(
			(_ws?: string, uuid?: string) =>
				(uuid === 'uuid-1'
					? firstProbe
					: Promise.resolve({ status: 'ok', mime: 'image/png', size: 4096 })) as never
		);
		editor = makeEditor(target);

		press(image(), 'Enter');
		expect(await opened()).toBe(0);

		editor.commands.setNodeSelection(1);
		editor.commands.updateAttributes('attachmentImage', { uuid: 'uuid-2' });
		await opened();
		editor.commands.setNodeSelection(1);
		editor.commands.updateAttributes('attachmentImage', { uuid: 'uuid-1' });
		await opened();
		// The premise: the ORIGINAL attachment really is back under the node.
		// Without it the stale request would be blocked by the uuid comparison
		// and this test would pass for the wrong reason.
		expect(image().getAttribute('data-attachment-id')).toBe('uuid-1');

		releaseFirst();

		expect(await opened()).toBe(0);
	});

	it('still opens an allowlisted raster type after the probe resolves', async () => {
		// The control for the refusal above: same code path, same probe, same
		// number of awaits — only the MIME differs.
		probeMock.mockResolvedValue({ status: 'ok', mime: 'image/png' , size: 4096 });
		editor = makeEditor(target);
		await settleProbe();
		expect(probeMock).toHaveBeenCalled();

		expect(image().getAttribute('role')).toBe('button');
		expect(openCount()).toBe(0);
		press(image(), 'Enter');
		expect(await opened()).toBe(1);
	});

	it('makes a deleted image inert rather than a dead focus stop', async () => {
		editor = makeEditor(target);
		const img = image();
		expect(img.getAttribute('role')).toBe('button');

		for (const fn of deletionListeners) fn('uuid-1');

		expect(img.getAttribute('role')).toBeNull();
		expect(img.getAttribute('tabindex')).toBeNull();
		expect(img.getAttribute('aria-label')).toBeNull();
		// Inert, not merely unreachable: activation itself refuses, so a
		// synthetic event or a stale focus can't open a viewer on a row that
		// no longer exists.
		press(img, 'Enter');
		click(img);
		expect(await opened()).toBe(0);
	});

	it('does not strand focus on an image that just went inert', () => {
		editor = makeEditor(target);
		const img = image();
		img.focus();
		expect(document.activeElement).toBe(img);

		for (const fn of deletionListeners) fn('uuid-1');

		// Removing tabindex from the focused element would otherwise leave focus
		// somewhere no further keystroke can leave.
		expect(document.activeElement).not.toBe(img);
	});

	it('gives a comment editor the same contract as an item body', async () => {
		// CommentEditor.svelte configures AttachmentImage independently, so
		// "works in the body" is not evidence it works in a comment.
		editor = makeCommentEditor(target);
		const img = image();

		expect(img.getAttribute('role')).toBe('button');
		expect(img.getAttribute('tabindex')).toBe('0');
		expect(img.getAttribute('aria-label')).toBe('View image: A diagram');

		expect(openCount()).toBe(0);
		press(img, 'Enter');
		expect(await opened()).toBe(1);
	});

	it('will not ACTIVATE while the image is showing a load-failure placeholder', async () => {
		// Attributes are not the contract — activation is. Stripping role and
		// tabindex hides the image from Tab, but a stale event still in flight, a
		// synthetic one, or focus that predates the failure all reach the
		// listeners directly, and there is nothing behind a transient failure to
		// open. Asserting only the attributes leaves this hole wide open, which
		// is precisely the shape of bug this phase keeps shipping.
		editor = makeEditor(target);
		const img = image();
		img.dispatchEvent(new Event('error'));

		press(img, 'Enter');
		press(img, ' ');
		click(img);

		expect(await opened()).toBe(0);
	});

	it('is not a focus stop while the image is showing a load-failure placeholder', () => {
		// The placeholder next to it carries the retry affordance; two controls
		// for one thing, one of which is invisible, is not a contract.
		editor = makeEditor(target);
		const img = image();
		img.dispatchEvent(new Event('error'));

		// The premise first: the image really did hand over to the placeholder.
		// Stripping the semantics off a still-visible image would be a different
		// (and wrong) implementation that these two assertions alone would accept.
		const placeholder = target.querySelector<HTMLElement>('.attachment-missing');
		expect(placeholder?.style.display).not.toBe('none');
		expect(img.style.display).toBe('none');

		expect(img.getAttribute('role')).toBeNull();
		expect(img.getAttribute('tabindex')).toBeNull();
	});
});
